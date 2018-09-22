// Copyright 2018 The Containerfs Authors.
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

package blob

import (
	"fmt"
	"github.com/juju/errors"
	"github.com/tiglabs/containerfs/proto"
	"github.com/tiglabs/containerfs/sdk/data/wrapper"
	"github.com/tiglabs/containerfs/util/log"
	"github.com/tiglabs/containerfs/util/pool"
	"hash/crc32"
	"net"
	"strings"
	"syscall"
)

const (
	MaxRetryCnt = 100
)

type BlobClient struct {
	cluster string
	volname string
	conns   *pool.ConnPool
	wraper  *wrapper.Wrapper
}

func NewBlobClient(volname, masters string) (*BlobClient, error) {
	client := new(BlobClient)
	client.volname = volname
	var err error
	client.conns = pool.NewConnPool()
	client.wraper, err = wrapper.NewDataPartitionWrapper(volname, masters)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (client *BlobClient) checkWriteResponse(request, reply *proto.Packet) (err error) {
	if reply.Opcode != proto.OpOk {
		return fmt.Errorf("WriteRequest(%v) reply(%v) replyOp Err msg(%v)",
			request.GetUniqueLogId(), reply.GetUniqueLogId(), string(reply.Data[:reply.Size]))
	}
	if request.ReqID != reply.ReqID {
		return fmt.Errorf("WriteRequest(%v) reply(%v) REQID not equare", request.GetUniqueLogId(), reply.GetUniqueLogId())
	}
	if request.PartitionID != reply.PartitionID {
		return fmt.Errorf("WriteRequest(%v) reply(%v) PartitionID not equare", request.GetUniqueLogId(), reply.GetUniqueLogId())
	}
	requestCrc := crc32.ChecksumIEEE(request.Data[:request.Size])
	if requestCrc != reply.Crc {
		return fmt.Errorf("WriteRequest(%v) reply(%v) CRC not equare,request(%v) reply(%v)", request.GetUniqueLogId(),
			reply.GetUniqueLogId(), requestCrc, reply.Crc)
	}

	return
}

func (client *BlobClient) checkReadResponse(request, reply *proto.Packet) (err error) {
	if reply.Opcode != proto.OpOk {
		return fmt.Errorf("ReadRequest(%v) reply(%v) replyOp Err msg(%v)",
			request.GetUniqueLogId(), reply.GetUniqueLogId(), string(reply.Data[:reply.Size]))
	}
	if request.ReqID != reply.ReqID {
		return fmt.Errorf("ReadRequest(%v) reply(%v) REQID not equare", request.GetUniqueLogId(), reply.GetUniqueLogId())
	}
	if request.PartitionID != reply.PartitionID {
		return fmt.Errorf("ReadRequest(%v) reply(%v) PartitionID not equare", request.GetUniqueLogId(), reply.GetUniqueLogId())
	}
	replyCrc := crc32.ChecksumIEEE(reply.Data[:reply.Size])
	if replyCrc != reply.Crc {
		return fmt.Errorf("ReadRequest(%v) reply(%v) CRC not equare,request(%v) reply(%v)", request.GetUniqueLogId(),
			reply.GetUniqueLogId(), request.Crc, replyCrc)
	}

	return
}

func (client *BlobClient) Write(data []byte) (key string, err error) {
	var (
		dp *wrapper.DataPartition
	)
	request := NewBlobWritePacket(dp, data)
	exclude := make([]uint32, 0)
	for i := 0; i < MaxRetryCnt; i++ {
		dp, err = client.wraper.GetWriteDataPartition(exclude)
		if err != nil {
			log.LogErrorf("Write: No write data partition")
			return "", syscall.ENOMEM
		}
		var (
			conn *net.TCPConn
		)
		if conn, err = client.conns.Get(dp.Hosts[0]); err != nil {
			log.LogWarnf("WriteRequest(%v) Get connect from host(%) err(%v)", request.GetUniqueLogId(), dp.Hosts[0], err.Error())
			exclude = append(exclude, dp.PartitionID)
			continue
		}
		if err = request.WriteToConn(conn); err != nil {
			client.conns.CheckErrorForPutConnect(conn, dp.Hosts[0], err)
			log.LogWarnf("WriteRequest(%v) Write to (%v) host(%) err(%v)", request.GetUniqueLogId(), dp.Hosts[0], err.Error())
			exclude = append(exclude, dp.PartitionID)
			continue
		}
		reply := new(proto.Packet)
		if err = reply.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
			client.conns.Put(conn, true)
			log.LogWarnf("WriteRequest(%v) Write (%v) host(%) err(%v)", request.GetUniqueLogId(), dp.Hosts[0], err.Error())
			exclude = append(exclude, dp.PartitionID)
			continue
		}
		if err = client.checkWriteResponse(request, reply); err != nil {
			client.conns.Put(conn, true)
			log.LogWarnf("WriteRequest CheckWriteResponse error(%v)", err.Error())
			exclude = append(exclude, dp.PartitionID)
			continue
		}
		partitionID, fileID, objID, size := ParsePacket(reply)
		client.conns.Put(conn, false)
		key = GenKey(client.cluster, client.volname, partitionID, fileID, objID, size)
		return key, nil
	}

	return "", syscall.EIO
}

func (client *BlobClient) Read(key string) (data []byte, err error) {
	cluster, volname, partitionID, fileID, objID, size, err := ParseKey(key)
	if err != nil || strings.Compare(cluster, client.cluster) != 0 || strings.Compare(volname, client.volname) != 0 {
		log.LogErrorf("Read: err(%v)", err)
		return nil, syscall.EINVAL
	}

	dp, err := client.wraper.GetDataPartition(partitionID)
	if dp == nil {
		log.LogErrorf("Read: No partition, key(%v) err(%v)", key, err)
		return
	}

	request := NewBlobReadPacket(partitionID, fileID, objID, size)
	for _, target := range dp.Hosts {
		var (
			conn *net.TCPConn
		)
		if conn, err = client.conns.Get(target); err != nil {
			err = errors.Annotatef(err, "ReadRequest(%v) Get connect from host(%)-", request.GetUniqueLogId(), target)
			client.conns.Put(conn, true)
			continue
		}
		if err = request.WriteToConn(conn); err != nil {
			client.conns.CheckErrorForPutConnect(conn, target, err)
			err = errors.Annotatef(err, "ReadRequest(%v) Write To host(%)-", request.GetUniqueLogId(), target)
			continue
		}
		reply := new(proto.Packet)
		if err = reply.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
			client.conns.Put(conn, true)
			err = errors.Annotatef(err, "ReadRequest(%v) ReadFrom host(%) err(%v)", request.GetUniqueLogId(), target)
			continue
		}
		if err = client.checkReadResponse(request, reply); err != nil {
			client.conns.Put(conn, true)
			err = errors.Annotatef(err, "ReadRequest CheckReadResponse", request.GetUniqueLogId(), target)
			continue
		}
		client.conns.Put(conn, false)
		return reply.Data, nil
	}

	return nil, syscall.EIO
}

func (client *BlobClient) Delete(key string) (err error) {
	cluster, volname, partitionID, fileID, objID, _, err := ParseKey(key)
	if err != nil || strings.Compare(cluster, client.cluster) != 0 || strings.Compare(volname, client.volname) != 0 {
		log.LogErrorf("Delete: err(%v)", err)
		return syscall.EINVAL
	}

	dp, err := client.wraper.GetDataPartition(partitionID)
	if dp == nil {
		log.LogErrorf("Delete: No partition, key(%v) err(%v)", key, err)
		return
	}
	request := NewBlobDeletePacket(dp, fileID, objID)
	var (
		conn *net.TCPConn
	)
	if conn, err = client.conns.Get(dp.Hosts[0]); err != nil {
		err = errors.Annotatef(err, "DeleteRequest(%v) Get connect from host(%)-", key, dp.Hosts[0])
		client.conns.Put(conn, true)
		return
	}
	if err = request.WriteToConn(conn); err != nil {
		client.conns.CheckErrorForPutConnect(conn, dp.Hosts[0], err)
		err = errors.Annotatef(err, "DeleteRequest(%v) Write To host(%)-", key, dp.Hosts[0])
		return
	}
	reply := new(proto.Packet)
	if err = reply.ReadFromConn(conn, proto.ReadDeadlineTime); err != nil {
		client.conns.Put(conn, true)
		err = errors.Annotatef(err, "DeleteRequest(%v) readResponse from host(%)-", key, dp.Hosts[0])
		return
	}
	if reply.Opcode != proto.OpOk {
		return fmt.Errorf("DeleteRequest(%v) reply(%v) replyOp Err msg(%v)",
			request.GetUniqueLogId(), reply.GetUniqueLogId(), string(reply.Data[:reply.Size]))
	}
	client.conns.Put(conn, false)

	return nil
}
