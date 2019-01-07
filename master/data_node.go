// Copyright 2018 The Container File System Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"math/rand"
	"sync"
	"time"

	"encoding/json"
	"github.com/tiglabs/containerfs/proto"
	"github.com/tiglabs/containerfs/util"
)

// DataNode stores all the information about a data node
type DataNode struct {
	Total      uint64 `json:"TotalWeight"`
	Used       uint64 `json:"UsedWeight"`
	Available  uint64 // TODO what is Available? 可用空间
	ID         uint64
	RackName   string `json:"Rack"`
	Addr       string
	ReportTime time.Time
	isActive   bool
	sync.RWMutex
	Ratio                float64 // 已用跟总共的比率
	SelectCount          uint64  // TODO what is SelectCount  这个dn被选择多少次 （选择作为dp的location）
	Carry                float64 // TODO what is Carry 磁盘使用率权重里面携带的因子 moosefs 改为 weight也行
	Sender               *AdminTaskSender // TODO explain? 专门处理发送任务的 （task mamanger）
	dataPartitionReports []*proto.PartitionReport
	DataPartitionCount   uint32
	NodeSetID            uint64
}

func newDataNode(addr, clusterID string) (dataNode *DataNode) {
	dataNode = new(DataNode)
	dataNode.Carry = rand.Float64()
	dataNode.Total = 1
	dataNode.Addr = addr
	dataNode.Sender = newAdminTaskSender(dataNode.Addr, clusterID)
	return
}

func (dataNode *DataNode) checkLiveness() {
	dataNode.Lock()
	defer dataNode.Unlock()
	if time.Since(dataNode.ReportTime) > time.Second*time.Duration(defaultNodeTimeOutSec) {
		dataNode.isActive = false
	}

	return
}

func (dataNode *DataNode) badPartitionIDs(disk string) (partitionIds []uint64) {
	partitionIds = make([]uint64, 0)
	dataNode.RLock()
	defer dataNode.RUnlock()
	for _, partitionReports := range dataNode.dataPartitionReports {
		if partitionReports.DiskPath == disk {
			partitionIds = append(partitionIds, partitionReports.PartitionID)
		}
	}

	return
}

func (dataNode *DataNode) updateNodeMetric(resp *proto.DataNodeHeartBeatResponse) {
	dataNode.Lock()
	defer dataNode.Unlock()
	dataNode.Total = resp.Total
	dataNode.Used = resp.Used
	dataNode.Available = resp.Available
	dataNode.RackName = resp.RackName
	dataNode.DataPartitionCount = resp.CreatedPartitionCnt
	dataNode.dataPartitionReports = resp.PartitionReports
	if dataNode.Total == 0 {
		dataNode.Ratio = 0.0
	} else {
		dataNode.Ratio = (float64)(dataNode.Used) / (float64)(dataNode.Total)
	}
	dataNode.ReportTime = time.Now()
	dataNode.isActive = true
}

func (dataNode *DataNode) isWriteAble() (ok bool) {
	dataNode.RLock()
	defer dataNode.RUnlock()

	if dataNode.isActive == true && dataNode.Available > util.DefaultDataPartitionSize {
		ok = true
	}

	return
}

func (dataNode *DataNode) isAvailCarryNode() (ok bool) {
	dataNode.RLock()
	defer dataNode.RUnlock()

	return dataNode.Carry >= 1
}

// SetCarry implements "SetCarry" in the Node interface
func (dataNode *DataNode) SetCarry(carry float64) {
	dataNode.Lock()
	defer dataNode.Unlock()
	dataNode.Carry = carry
}

// SelectNodeForWrite implements "SelectNodeForWrite" in the Node interface
// TODO explain
func (dataNode *DataNode) SelectNodeForWrite() {
	dataNode.Lock()
	defer dataNode.Unlock()
	dataNode.Ratio = float64(dataNode.Used) / float64(dataNode.Total)
	dataNode.SelectCount++
	dataNode.Carry = dataNode.Carry - 1.0
}

// TODO rename clear()?
func (dataNode *DataNode) clean() {
	dataNode.Sender.exitCh <- struct{}{}
}

func (dataNode *DataNode) createHeartbeatTask(masterAddr string) (task *proto.AdminTask) {
	request := &proto.HeartBeatRequest{
		CurrTime:   time.Now().Unix(),
		MasterAddr: masterAddr,
	}
	task = proto.NewAdminTask(proto.OpDataNodeHeartbeat, dataNode.Addr, request)
	return
}

func (dataNode *DataNode) toJSON() (body []byte, err error) {
	dataNode.RLock()
	defer dataNode.RUnlock()
	return json.Marshal(dataNode)
}
