package handler

import (
	"github.com/lengzhao/govm/conf"
	core "github.com/lengzhao/govm/core"
	"github.com/lengzhao/govm/event"
	"github.com/lengzhao/govm/messages"
	"github.com/lengzhao/govm/runtime"
	"github.com/lengzhao/govm/wallet"
	"log"
	"math/rand"
	"time"
)

func getTransListForMine(chain uint64) ([]core.Hash, uint64) {
	var preKey []byte
	var size uint64
	out := make([]core.Hash, 0)
	limit := core.GetBlockSizeLimit(chain)
	t := core.GetBlockTime(chain)
	for {
		trans := getNextTransInfo(chain, preKey)
		if trans.Key.Empty() {
			break
		}
		log.Printf("mine.chain:%d,trans:%x\n", chain, trans.Key)
		preKey = trans.Key[:]
		if size+uint64(trans.Size) > limit {
			log.Println("size > limit.break")
			break
		}
		if trans.Time > t {
			deleteTransInfo(chain, trans.Key[:])
			continue
		}
		if trans.Time+transAcceptTime < t {
			deleteTransInfo(chain, trans.Key[:])
			continue
		}
		_, err := core.CheckTransaction(chain, trans.Key[:])
		if err != nil {
			deleteTransInfo(chain, trans.Key[:])
			log.Printf("error trans.chain:%d,key:%x,err:%s\n", chain, trans.Key, err)
			continue
		}
		trans.Selected++
		if trans.Selected > 1 {
			if trans.Ops == core.OpsRunApp || trans.Ops == core.OpsNewChain {
				deleteTransInfo(chain, trans.Key[:])
				continue
			}
		}

		if trans.Selected > 3 {
			deleteTransInfo(chain, trans.Key[:])
			continue
		}
		saveTransInfo(chain, trans.Key[:], trans)

		out = append(out, trans.Key)
		size += uint64(trans.Size)
	}
	return out, size
}

func doMine(chain uint64, force bool) {
	c := conf.GetConf()
	if !c.DoMine {
		return
	}
	if c.ChainOfMine != 0 && c.ChainOfMine != chain {
		return
	}

	select {
	case procMgr.mineLock <- 1:
		log.Println("start to doMine:", chain)
	default:
		return
	}
	defer func() {
		<-procMgr.mineLock
		log.Println("finish doMine:", chain)
	}()

	addr := core.Address{}
	runtime.Decode(c.WalletAddr, &addr)
	block := core.NewBlock(chain, addr)

	if !force {
		count := GetMineCount(chain, block.Previous[:])
		if count > 1 {
			return
		}
		SetMineCount(chain, block.Previous[:], count+1)
	}

	transList, size := getTransListForMine(chain)

	block.SetTransList(transList)
	block.Size = uint32(size)
	block.Nonce = rand.Uint64()

	var oldRel core.TReliability

	timeout := time.Now().Unix() + 20

	for {
		now := time.Now().Unix()
		if timeout < now && block.Time < uint64(now)*1000 {
			break
		}
		signData := block.GetSignData()
		sign := wallet.Sign(c.PrivateKey, signData)
		if len(sign) == 0 {
			continue
		}
		if len(c.SignPrefix) > 0 {
			s := make([]byte, len(c.SignPrefix))
			copy(s, c.SignPrefix)
			sign = append(s, sign...)
		}

		block.SetSign(sign)
		data := block.Output()

		hp := getHashPower(block.Key[:])
		if hp <= block.HashpowerLimit {
			block.Nonce++
			// log.Printf("drop hash:%x,data:%x\n", key, signData[:6])
			continue
		}
		rel := block.GetReliability()
		if rel.Cmp(oldRel) > 0 {
			oldRel = rel
			core.WriteBlock(chain, data)
			core.SaveBlockReliability(chain, block.Key[:], rel)
			info := messages.BlockInfo{}
			info.Chain = chain
			info.Index = rel.Index
			info.Key = rel.Key[:]
			info.HashPower = rel.HashPower
			info.PreKey = rel.Previous[:]
			network.SendInternalMsg(&messages.BaseMsg{Type: messages.BroadcastMsg, Msg: &info})
			log.Printf("mine one blok,chain:%d,index:%d,hashpower:%d,hp limit:%d,key:%x\n",
				chain, rel.Index, rel.HashPower, block.HashpowerLimit, rel.Key)
		}
	}

	if oldRel.HashPower == 0 {
		log.Printf("fail to doMine,error oldHP,limit:%d\n", block.HashpowerLimit)
		count := GetMineCount(chain, block.Previous[:])
		if count > 0 {
			SetMineCount(chain, block.Previous[:], count-1)
		}
		return
	}
}

func autoRegisterMiner(chain uint64) {
	c := conf.GetConf()
	if c.CostOfRegMiner < 100 {
		return
	}
	if chain != c.ChainOfMine && c.ChainOfMine != 0 {
		return
	}
	cost := core.GetUserCoin(chain, c.WalletAddr)
	if cost < c.CostOfRegMiner {
		return
	}
	index := core.GetLastBlockIndex(chain)
	index += 50
	miner := core.GetMinerInfo(chain, index)
	if c.CostOfRegMiner < miner.Cost[5] {
		return
	}
	cAddr := core.Address{}
	runtime.Decode(c.WalletAddr, &cAddr)
	trans := core.NewTransaction(chain, cAddr)
	trans.Time = core.GetBlockTime(chain)
	trans.CreateRegisterMiner(0, index, c.CostOfRegMiner)
	trans.Energy = c.EnergyLimitOfMine
	td := trans.GetSignData()
	sign := wallet.Sign(c.PrivateKey, td)
	if len(c.SignPrefix) > 0 {
		s := make([]byte, len(c.SignPrefix))
		copy(s, c.SignPrefix)
		sign = append(s, sign...)
	}
	trans.SetSign(sign)
	td = trans.Output()

	msg := new(messages.NewTransaction)
	msg.Chain = chain
	msg.Key = trans.Key[:]
	msg.Data = td
	event.Send(msg)
	// log.Println("SendInternalMsg autoRegisterMiner:", msg)
}