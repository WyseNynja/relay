/*

  Copyright 2017 Loopring Project Ltd (Loopring Foundation).

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.

*/

package bucket

import (
	"github.com/Loopring/ringminer/log"
	"github.com/Loopring/ringminer/miner"
	"github.com/Loopring/ringminer/types"
	"math/rand"
	"strconv"
	"sync"
)

//负责生成ring，并计算ring相关的所有参数
//按照首字母，对未成环的进行存储
//逻辑为：订单会发送给每一个bucket，每个bucket，根据结尾的coin，进行链接，
//订单开始的coin为对应的bucket的标号时，查询订单结尾的coin的bucket，并进行对应的链接
//同时会监听proxy发送过来的订单环，及时进行订单的删除与修改
//应当尝试更改为node，提高内存的利用率

//todo：环的最大长度
const RingLength = 4

type OrderWithPos struct {
	types.OrderState
	postions []*semiRingPos
}

type semiRingPos struct {
	semiRingKey string //可以到达的途径
	index       int    //所在的数组索引
}

type SemiRing struct {
	orders []*OrderWithPos //组成该半环的node
	hash   string
	finish bool
	//reduction reductionOrder 	//半环组成的规约后的新的order
}

func (r *SemiRing) hashFunc() string {
	//todo:just for test
	return strconv.Itoa(rand.Int())
}

type Bucket struct {
	ringChan  chan *types.RingState
	token     types.Address                //开始的地址
	semiRings map[string]*SemiRing         //每个semiRing都给定一个key
	orders    map[types.Hash]*OrderWithPos //order hash -> order
	mtx       *sync.RWMutex
}

//新bucket
func NewBucket(token types.Address, ringChan chan *types.RingState) *Bucket {
	bucket := &Bucket{}
	bucket.token = token
	bucket.ringChan = ringChan
	bucket.orders = make(map[types.Hash]*OrderWithPos)
	bucket.semiRings = make(map[string]*SemiRing)
	bucket.mtx = &sync.RWMutex{}
	return bucket
}

func convertOrderStateToFilledOrder(order *types.OrderState) *types.FilledOrder {
	filledOrder := &types.FilledOrder{}
	filledOrder.OrderState = *order
	return filledOrder
}

func (b *Bucket) generateRing(order *types.OrderState) {
	var ring *types.RingState
	for _, semiRing := range b.semiRings {
		lastOrder := semiRing.orders[len(semiRing.orders)-1]

		//是否与最后一个订单匹配，匹配则可成环
		if lastOrder.RawOrder.TokenB == order.RawOrder.TokenS {

			ringTmp := &types.RingState{}
			ringTmp.RawRing = &types.Ring{}

			ringTmp.RawRing.Orders = []*types.FilledOrder{}

			for _, o := range semiRing.orders {
				ringTmp.RawRing.Orders = append(ringTmp.RawRing.Orders, convertOrderStateToFilledOrder(&o.OrderState))
			}
			ringTmp.RawRing.Orders = append(ringTmp.RawRing.Orders, convertOrderStateToFilledOrder(order))
			//兑换率是否匹配
			if miner.PriceValid(ringTmp) {
				miner.ComputeRing(ringTmp) //计算兑换的费用、折扣率等，便于计算收益，选择最大环
				log.Debugf("bucket:%s, len:%d, fee:%d, order.idx:%s", b.token.Str(), len(b.orders), ringTmp.LegalFee.RealValue().Int64(), semiRing.orders[0].Hash.Str())
				//选择收益最大的环
				if ring == nil ||
					ringTmp.LegalFee.Cmp(ring.LegalFee) > 0 ||
					(ringTmp.LegalFee.Cmp(ring.LegalFee) == 0 && len(ringTmp.RawRing.Orders) < len(ring.RawRing.Orders)) {
					ringTmp.Hash = miner.Hash(ringTmp)
					ring = ringTmp
				}
			}
		}
	}

	//todo：生成新环后，需要proxy将新环对应的各个订单的状态发送给每个bucket，便于修改，, 还有一些过滤条件
	//删除对应的semiRing，转到等待proxy通知，但是会暂时标记该半环
	if ring != nil {
		//b.newRingWithoutLock(ring)
		b.ringChan <- ring
	}

}

func (b *Bucket) generateSemiRing(order *types.OrderState) {
	orderWithPos := &OrderWithPos{}
	orderWithPos.OrderState = *order

	//首先生成包含自己的semiRing
	selfSemiRing := &SemiRing{}
	selfSemiRing.orders = []*OrderWithPos{orderWithPos}
	selfSemiRing.hash = selfSemiRing.hashFunc()
	pos := &semiRingPos{semiRingKey: selfSemiRing.hash, index: len(selfSemiRing.orders)}
	orderWithPos.postions = []*semiRingPos{pos}
	b.orders[orderWithPos.Hash] = orderWithPos
	b.semiRings[selfSemiRing.hash] = selfSemiRing

	//新半环列表
	semiRingMap := make(map[string]*SemiRing)

	//相等的话，则为第一层，下面每一层都加过来
	for _, semiRing := range b.semiRings {
		//兑换率判断
		lastOrder := semiRing.orders[len(semiRing.orders)-1]

		if lastOrder.RawOrder.TokenS == order.RawOrder.TokenB {

			semiRingNew := &SemiRing{}
			semiRingNew.orders = append(selfSemiRing.orders, semiRing.orders[1:]...)
			semiRingNew.hash = semiRingNew.hashFunc()

			semiRingMap[semiRingNew.hash] = semiRingNew

			//修改每个订单中保存的semiRing的信息
			for idx, order1 := range semiRingNew.orders {
				//生成新的semiring,
				order1.postions = append(order1.postions, &semiRingPos{semiRingKey: semiRingNew.hash, index: idx})
			}
		}
	}

	for k, v := range semiRingMap {
		b.semiRings[k] = v
	}
}

func (b *Bucket) appendToSemiRing(order *types.OrderState) {
	semiRingMap := make(map[string]*SemiRing)

	//第二层以下，只检测最后的token 即可
	for _, semiRing := range b.semiRings {
		lastOrder := semiRing.orders[len(semiRing.orders)-1]

		if (len(semiRing.orders) < RingLength) && lastOrder.RawOrder.TokenB == order.RawOrder.TokenS {

			orderWithPos := &OrderWithPos{}
			orderWithPos.OrderState = *order
			orderWithPos.postions = []*semiRingPos{}
			b.orders[orderWithPos.Hash] = orderWithPos

			semiRingNew := &SemiRing{}
			semiRingNew.orders = append(semiRing.orders, orderWithPos)
			semiRingNew.hash = semiRingNew.hashFunc()

			semiRingMap[semiRingNew.hash] = semiRingNew

			for idx, o1 := range semiRingNew.orders {
				o1.postions = append(o1.postions, &semiRingPos{semiRingKey: semiRingNew.hash, index: idx})
			}
		}
	}
	for k, v := range semiRingMap {
		b.semiRings[k] = v
	}
}

func (b *Bucket) NewOrder(ord types.OrderState) {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	b.newOrderWithoutLock(ord)
}

func (b *Bucket) DeleteOrder(ord types.OrderState) {
	//delete the order
	b.mtx.RLock()
	defer b.mtx.RUnlock()

	if o, ok := b.orders[ord.Hash]; ok {
		for _, pos := range o.postions {
			delete(b.semiRings, pos.semiRingKey)
		}
		delete(b.orders, ord.Hash)
	}

}

func (b *Bucket) Start() {

}

func (b *Bucket) Stop() {

}

//this fun should not be called without mtx.lock()
func (b *Bucket) newOrderWithoutLock(ord types.OrderState) {
	//if orders contains this order, there are nothing to do
	if _, ok := b.orders[ord.Hash]; !ok {
		//最后一个token为当前token，则可以组成环，匹配出最大环，并发送到proxy
		if ord.RawOrder.TokenB == b.token {
			log.Debugf("bucket receive order:%s", ord.Hash.Hex())

			b.generateRing(&ord)
		} else if ord.RawOrder.TokenS == b.token {
			//卖出的token为当前token时，需要将所有的买入semiRing加入进来
			b.generateSemiRing(&ord)
		} else {
			//其他情况
			b.appendToSemiRing(&ord)
		}
	}
}