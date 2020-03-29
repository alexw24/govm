package handler

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/lengzhao/govm/conf"
	core "github.com/lengzhao/govm/core"
	"github.com/lengzhao/govm/messages"
	"github.com/lengzhao/govm/runtime"
)

// chain->index->blockKey->reliability
type tProcessMgr struct {
	mu       sync.Mutex
	stop     bool
	wait     map[uint64]chan int
	Chains   map[uint64]chan int
	procTime map[uint64]uint64
	mineLock map[uint64]chan int
}

const (
	tSecond           = 1000
	tMinute           = 60 * 1000
	tHour             = 60 * tMinute
	tDay              = 24 * tHour
	blockAcceptTime   = 2 * tMinute
	transAcceptTime   = 9 * tDay
	blockSyncTime     = 5 * tMinute
	hpAcceptRange     = 20
	blockLockInterval = 6
	lockBySelfChain   = 1
)

var procMgr tProcessMgr

func init() {
	procMgr.wait = make(map[uint64]chan int)
	procMgr.Chains = make(map[uint64]chan int)
	procMgr.mineLock = make(map[uint64]chan int)
	procMgr.procTime = make(map[uint64]uint64)

	time.AfterFunc(time.Second*5, timeoutFunc)
}

func timeoutFunc() {
	if procMgr.stop {
		log.Println("procMgr.stop")
		return
	}
	time.AfterFunc(time.Second*20, timeoutFunc)
	processChains(1)
}

func processChains(chain uint64) {
	id := core.GetLastBlockIndex(chain)
	if id == 0 {
		return
	}
	go processEvent(chain)
	processChains(2 * chain)
	processChains(2*chain + 1)
}

func getBestBlock(chain, index uint64) TReliability {
	var relia TReliability
	ib := ReadIDBlocks(chain, index)
	now := uint64(time.Now().Unix() * 1000)
	t := core.GetBlockTime(chain)
	for i, it := range ib.Items {
		key := it.Key[:]
		rel := ReadBlockReliability(chain, key)

		// time.Second
		if rel.Time > now {
			continue
		}
		if index != rel.Index {
			log.Printf("error index of block,hope:%d,get:%d\n", index, rel.Index)
			rel.Index = 0
			SaveBlockReliability(chain, key, rel)
			setBlockToIDBlocks(chain, index, it.Key, 0)
			core.DeleteBlock(chain, key)
			continue
		}
		hp := rel.HashPower
		if t+blockSyncTime > now {
			if !rel.Parent.Empty() && !core.BlockOnTheChain(chain/2, rel.Parent[:]) {
				continue
			}
			if !rel.LeftChild.Empty() && !core.BlockOnTheChain(chain*2, rel.LeftChild[:]) {
				continue
			}
			if !rel.RightChild.Empty() && !core.BlockOnTheChain(chain*2+1, rel.RightChild[:]) {
				continue
			}
		}

		stat := ReadBlockRunStat(chain, key)
		if index > 1 && t+blockSyncTime < now {
			bln := getBlockLockNum(chain, key)
			hp += bln
		}

		hp -= stat.SelectedCount / 5
		hp -= uint64(stat.RunTimes) / 10
		hp -= uint64(stat.RunTimes - stat.RunSuccessCount)
		if hp == 0 {
			setBlockToIDBlocks(chain, index, it.Key, 0)
			core.DeleteBlock(chain, it.Key[:])
			stat = BlockRunStat{}
			SaveBlockRunStat(chain, it.Key[:], stat)
			SaveBlockReliability(chain, key, TReliability{})
			continue
		}

		if stat.SelectedCount > 1 || stat.RollbackCount > 1 {
			log.Printf("getBestBlock,chain:%d,index:%d,key:%x,i:%d,hp:%d,"+
				"rollback:%d,runTimes:%d,success:%d,selected:%d\n",
				chain, index, key, i, rel.HashPower, stat.RollbackCount,
				stat.RunTimes, stat.RunSuccessCount, stat.SelectedCount)
		}

		rel.HashPower = hp
		if rel.Cmp(relia) > 0 {
			relia = rel
		}
	}
	if !relia.Key.Empty() {
		log.Printf("getBestBlock rst,num:%d,chain:%d,index:%d,hp:%d,key:%x\n", len(ib.Items),
			chain, index, relia.HashPower, relia.Key)
	}

	return relia
}

func checkAndRollback(chain, index uint64, key []byte) bool {
	cInfo := core.GetChainInfo(chain)
	t1 := core.GetBlockTime(chain)
	if cInfo.ParentID > 1 {
		t0 := core.GetBlockTime(chain / 2)
		if t0 > t1+blockSyncTime {
			log.Printf("[warn]chain:%d,index:%d. parent-self.Time:%ds,key:%x\n", chain, index, t0-t1/tSecond, key)
			go processEvent(chain / 2)
			return false
		}
	}
	if cInfo.LeftChildID > 1 {
		t2 := core.GetBlockTime(chain * 2)
		if t2 > t1+blockSyncTime {
			log.Printf("[warn]chain:%d,index:%d. leftChild-self.Time:%ds,key:%x\n", chain, index, t2-t1/tSecond, key)
			go processEvent(chain * 2)
			return false
		}
	}
	if cInfo.RightChildID > 1 {
		t3 := core.GetBlockTime(chain*2 + 1)
		if t3 > t1+blockSyncTime {
			log.Printf("[warn]chain:%d,index:%d. rightChild-self.Time:%ds,key:%x\n", chain, index, t3-t1/tSecond, key)
			go processEvent(chain*2 + 1)
			return false
		}
	}
	dbRollBack(chain, index, key)
	return true
}

func checkOtherChain(chain uint64) error {
	index := core.GetLastBlockIndex(chain)
	cInfo := core.GetChainInfo(chain)
	if chain > 1 && index > 0 && index < 100 {
		// parent chain rollback,the block(new chain) is not exist
		pk := core.GetParentBlockOfChain(chain)
		if !pk.Empty() && !core.BlockOnTheChain(chain/2, pk[:]) {
			log.Printf("rollback chain:%d,index:%d,hope:%x on parent chain\n", chain, index, pk)
			var i uint64
			ib := IDBlocks{}
			for i = 2; i < index+1; i++ {
				SaveIDBlocks(chain, i, ib)
			}
			lk := core.GetTheBlockKey(chain, 1)
			dbRollBack(chain, 1, lk)
			return errors.New("different parent")
		}
	}
	if cInfo.LeftChildID == 1 {
		go writeFirstBlockToChain(chain * 2)
	} else if cInfo.RightChildID == 1 {
		go writeFirstBlockToChain(chain*2 + 1)
	}

	// chain1.Time-chain2.Time > blockSyncTime, stop
	t1 := core.GetBlockTime(chain)
	if cInfo.ParentID > 1 {
		t0 := core.GetBlockTime(chain / 2)
		if t1 > t0+blockSyncTime {
			log.Printf("wait chain:%d,index:%d. parent.Time:%d,self.Time:%d\n", chain, index, t0, t1)
			go processEvent(chain / 2)
			return errors.New("wait parent chain")
		}
	}
	if cInfo.LeftChildID > 1 {
		t2 := core.GetBlockTime(chain * 2)
		if t1 > t2+blockSyncTime {
			log.Printf("wait chain:%d,index:%d. leftChild.Time:%d,self.Time:%d\n", chain, index, t2, t1)
			go processEvent(chain * 2)
			return errors.New("wait LeftChild chain")
		}
	}
	if cInfo.RightChildID > 1 {
		t3 := core.GetBlockTime(chain*2 + 1)
		if t1 > t3+blockSyncTime {
			log.Printf("wait chain:%d,index:%d. rightChild.Time:%d,self.Time:%d\n", chain, index, t3, t1)
			go processEvent(chain*2 + 1)
			return errors.New("wait RightChild chain")
		}
	}

	return nil
}

func beforeProcBlock(chain uint64, rel TReliability) error {
	id := core.GetLastBlockIndex(chain)
	preKey := core.GetTheBlockKey(chain, 0)
	if bytes.Compare(rel.Previous[:], preKey) == 0 {
		return nil
	}

	if !rel.Previous.Empty() && !core.IsExistBlock(chain, rel.Previous[:]) {
		log.Printf("not exist previous,chain:%d,index:%d,key:%x\n",
			chain, rel.Index-1, rel.Previous)
		rel.Index = 0
		SaveBlockReliability(chain, rel.Key[:], rel)
		setBlockToIDBlocks(chain, rel.Index, rel.Key, 0)
		core.DeleteBlock(chain, rel.Key[:])
		return errors.New("not previous")
	}

	bln := getBlockLockNum(chain, rel.Key[:])
	setBlockLockNum(chain, rel.Previous[:], bln+1)
	t := core.GetBlockTime(chain)
	interval := core.GetBlockInterval(chain)
	now := uint64(time.Now().Unix() * 1000)
	if t+interval*3/2 > now {
		// Need rollback,it could not be the newest block. Prevention of attacks
		return errors.New("need rollback,but block too new")
	}
	if rel.Time < t+interval/2 {
		setBlockToIDBlocks(chain, rel.Index, rel.Key, 0)
		return errors.New("error block time")
	}
	if rel.Index < id {
		setBlockToIDBlocks(chain, rel.Index, rel.Key, 0)
		core.DeleteBlock(chain, rel.Key[:])
		SaveBlockReliability(chain, rel.Key[:], TReliability{})
		return errors.New("error rel.Index")
	}

	if checkAndRollback(chain, id, preKey) {
		log.Printf("dbRollBack block. index:%d,key:%x,next block:%x\n", rel.Index, preKey, rel.Key)
		ib := IDBlocks{}
		it := ItemBlock{rel.Previous, 1}
		ib.Items = append(ib.Items, it)
		SaveIDBlocks(chain, rel.Index-1, ib)
	}
	go processEvent(chain)

	return errors.New("rollback")
}

func finishProcBlock(chain uint64, rel TReliability, rn int) error {
	if rel.Index < 1 {
		return fmt.Errorf("error index")
	}
	if !rel.LeftChild.Empty() {
		setBlockLockNum(chain*2, rel.LeftChild[:], 10)
	}
	if !rel.RightChild.Empty() {
		setBlockLockNum(chain*2+1, rel.RightChild[:], 10)
	}

	old := rel.HashPower
	rel.Recalculation(chain)
	if old != rel.HashPower {
		SaveBlockReliability(chain, rel.Key[:], rel)
	}
	return nil
}

func processEvent(chain uint64) {
	if chain == 0 {
		return
	}
	if network == nil {
		return
	}
	if procMgr.stop {
		log.Println("procMgr.stop")
		return
	}

	defer func() {
		e := recover()
		if e != nil {
			log.Println("something error,", e)
			log.Println(string(debug.Stack()))
		}
	}()

	procMgr.mu.Lock()
	wait, ok := procMgr.wait[chain]
	if !ok {
		procMgr.wait[chain] = make(chan int, 2)
		wait = procMgr.wait[chain]
	}
	cl, ok := procMgr.Chains[chain]
	if !ok {
		procMgr.Chains[chain] = make(chan int, 1)
		cl = procMgr.Chains[chain]
	}
	procMgr.mu.Unlock()

	select {
	case wait <- 1:
	default:
		return
	}
	defer func() { <-wait }()

	cl <- 1
	// log.Println("start processEvent:", chain)
	defer func() {
		log.Println("finish processEvent:", chain)
		<-cl
	}()

	index := core.GetLastBlockIndex(chain)
	// check child chain
	err := checkOtherChain(chain)
	if err != nil {
		log.Printf("checkOtherChain,rollback,chain:%d,err:%s\n", chain, err)
		lk := core.GetTheBlockKey(chain, index)
		dbRollBack(chain, index, lk)
		return
	}

	var relia TReliability
	now := uint64(time.Now().Unix() * 1000)

	relia = getBestBlock(chain, index+1)
	if relia.Key.Empty() {
		relia = getBestBlock(chain, index)
		nowKey := core.GetTheBlockKey(chain, index)
		if !relia.Key.Empty() && bytes.Compare(relia.Key[:], nowKey) != 0 {
			log.Printf("dbRollBack block. index:%d,key:%x,next block:%x\n", index, nowKey, relia.Key)
			checkAndRollback(chain, index, nowKey)
			return
		}
		t := core.GetBlockTime(chain)
		go doMine(chain, false)

		if t+core.GetBlockInterval(chain) >= now {
			return
		}
		log.Printf("no next block key,chain:%d,index:%d\n", chain, index+1)
		procMgr.mu.Lock()
		procTime := procMgr.procTime[chain]
		procMgr.mu.Unlock()
		if procTime+blockSyncTime < now {
			// not next block long time,rollback
			procMgr.mu.Lock()
			procMgr.procTime[chain] = now - blockSyncTime + tMinute
			procMgr.mu.Unlock()
			dbRollBack(chain, index, nowKey)
			go processEvent(chain)
			return
		}
		info := &messages.ReqBlockInfo{Chain: chain, Index: index + 1}
		if activeNode != nil {
			activeNode.Send(info)
		}
		if t+10*tMinute < now {
			info = &messages.ReqBlockInfo{Chain: chain, Index: index + 10}
			finishProcBlock(chain, relia, 10)
		}
		if needRequstID(chain, info.Index) {
			network.SendInternalMsg(&messages.BaseMsg{Type: messages.RandsendMsg, Msg: info})
		}
		return
	}
	stat := ReadBlockRunStat(chain, relia.Key[:])
	stat.SelectedCount++
	//
	err = beforeProcBlock(chain, relia)
	if err != nil {
		log.Println("beforeProcBlock,chain:", chain, err)
		SaveBlockRunStat(chain, relia.Key[:], stat)
		return
	}
	stat.RunTimes++
	log.Printf("start to process block,chain:%d,index:%d,key:%x\n", chain, relia.Index, relia.Key)
	err = core.ProcessBlockOfChain(chain, relia.Key[:])
	if err != nil {
		log.Printf("fail to process block,chain:%d,index:%d,key:%x,error:%s\n", chain, index+1, relia.Key, err)
		SaveBlockRunStat(chain, relia.Key[:], stat)
		setBlockToIDBlocks(chain, relia.Index, relia.Key, 0)
		saveBlackItem(chain, relia.Producer[:])
		return
	}
	procMgr.mu.Lock()
	procMgr.procTime[chain] = now
	procMgr.mu.Unlock()
	stat.RunSuccessCount++
	SaveBlockRunStat(chain, relia.Key[:], stat)

	finishProcBlock(chain, relia, stat.RunSuccessCount)

	if relia.Time+blockSyncTime < now {
		go processEvent(chain)
		return
	}
	autoRegisterMiner(chain)

	info := messages.BlockInfo{}
	info.Chain = chain
	info.Index = relia.Index
	info.Key = relia.Key[:]
	info.HashPower = relia.HashPower
	info.PreKey = relia.Previous[:]
	network.SendInternalMsg(&messages.BaseMsg{Type: messages.BroadcastMsg, Msg: &info})

	go processEvent(chain)
}

func writeFirstBlockToChain(chain uint64) {
	if chain <= 1 {
		return
	}
	id := core.GetLastBlockIndex(chain)
	if id > 0 {
		return
	}
	c := conf.GetConf()
	data := core.ReadTransactionData(1, c.FirstTransName)
	if len(data) > 0 {
		core.WriteTransaction(chain, data)
	}
	key := core.GetTheBlockKey(1, 1)
	data = core.ReadBlockData(1, key)
	processBlock(chain, key, data)
	var k core.Hash
	runtime.Decode(key, &k)
	setBlockToIDBlocks(chain, 1, k, 1000)
	log.Println("new chain:", chain)
	go processEvent(chain)
}

func getHashPower(in []byte) uint64 {
	var out uint64
	for _, item := range in {
		out += 8
		if item != 0 {
			for item > 0 {
				out--
				item = item >> 1
			}
			return out
		}
	}
	return out
}

// hp=0,delete;hp>1,add and update; hp=1,add
func setBlockToIDBlocks(chain, index uint64, key core.Hash, hp uint64) {
	if key.Empty() {
		return
	}
	ib := ReadIDBlocks(chain, index)
	var newIB IDBlocks
	for _, it := range ib.Items {
		if key == it.Key {
			if hp > it.HashPower {
				it.HashPower = hp
			}
			if hp > 0 {
				newIB.Items = append(newIB.Items, it)
			}
			hp = 0
			continue
		}
		if hp > it.HashPower {
			// log.Printf("IDBlocks switch,index:%d,i:%d,old:%x,new:%x\n", block.Index, i, b, key)
			nit := ItemBlock{Key: key, HashPower: hp}
			newIB.Items = append(newIB.Items, nit)
			newIB.Items = append(newIB.Items, it)
			hp = 0
		} else {
			newIB.Items = append(newIB.Items, it)
		}
	}
	if hp > 0 {
		nit := ItemBlock{Key: key, HashPower: hp}
		newIB.Items = append(newIB.Items, nit)
	}
	if len(newIB.Items) > core.MinerNum {
		newIB.Items = newIB.Items[:core.MinerNum]
	}
	// newIB.MaxHeight = ib.MaxHeight
	SaveIDBlocks(chain, index, newIB)
}
