// Copyright 2022 CeresDB Project Authors. Licensed under Apache-2.0.

package cluster

import (
	"context"
	"sync"

	"github.com/CeresDB/ceresdbproto/pkg/clusterpb"
	"github.com/CeresDB/ceresmeta/pkg/log"
	"github.com/CeresDB/ceresmeta/server/id"
	"github.com/CeresDB/ceresmeta/server/schedule"
	"github.com/CeresDB/ceresmeta/server/storage"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	AllocClusterIDPrefix = "ClusterID"
	AllocSchemaIDPrefix  = "SchemaID"
	AllocTableIDPrefix   = "TableID"
)

type TableInfo struct {
	ID         uint64
	Name       string
	SchemaID   uint32
	SchemaName string
}

type ShardTables struct {
	ShardRole clusterpb.ShardRole
	Tables    []*TableInfo
	Version   uint64
}

type Manager interface {
	CreateCluster(ctx context.Context, clusterName string, nodeCount, replicationFactor, shardTotal uint32) (*Cluster, error)
	AllocSchemaID(ctx context.Context, clusterName, schemaName string) (uint32, error)
	AllocTableID(ctx context.Context, clusterName, schemaName, tableName, nodeName string) (*Table, error)
	GetTables(ctx context.Context, clusterName, nodeName string, shardIDs []uint32) (map[uint32]*ShardTables, error)
	DropTable(ctx context.Context, clusterName, schemaName, tableName string, tableID uint64) error
	RegisterNode(ctx context.Context, clusterName, nodeName string, lease uint32) error
	GetShards(ctx context.Context, clusterName, nodeName string) ([]uint32, error)
}

type managerImpl struct {
	// RWMutex is used to protect clusters when creating new cluster
	lock     sync.RWMutex
	clusters map[string]*Cluster

	storage   storage.Storage
	alloc     id.Allocator
	hbstreams *schedule.HeartbeatStreams
	rootPath  string
}

func NewManagerImpl(ctx context.Context, storage storage.Storage, hbstream *schedule.HeartbeatStreams, rootPath string) (Manager, error) {
	alloc := id.NewAllocatorImpl(storage, rootPath, AllocClusterIDPrefix)

	manager := &managerImpl{storage: storage, alloc: alloc, clusters: make(map[string]*Cluster, 0), hbstreams: hbstream, rootPath: rootPath}

	clusters, err := manager.storage.ListClusters(ctx)
	if err != nil {
		log.Error("new clusters manager failed, fail to list clusters", zap.Error(err))
		return nil, errors.Wrap(err, "clusters manager list clusters")
	}

	manager.lock.Lock()
	defer manager.lock.Unlock()

	manager.clusters = make(map[string]*Cluster, len(clusters))
	for _, clusterPb := range clusters {
		cluster := NewCluster(clusterPb, manager.storage, manager.hbstreams, manager.rootPath)
		if err := cluster.Load(ctx); err != nil {
			log.Error("new clusters manager failed, fail to load cluster", zap.Error(err))
			return nil, errors.Wrapf(err, "clusters manager Load, clusters:%v", cluster)
		}
		manager.clusters[cluster.Name()] = cluster
	}

	return manager, nil
}

func (m *managerImpl) CreateCluster(ctx context.Context, clusterName string, initialNodeCount,
	replicationFactor, shardTotal uint32,
) (*Cluster, error) {
	if initialNodeCount < 1 {
		log.Error("cluster's nodeCount must > 0", zap.String("clusterName", clusterName))
		return nil, ErrCreateCluster.WithCausef("nodeCount must > 0")
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	_, ok := m.clusters[clusterName]
	if ok {
		log.Error("cluster already exists", zap.String("clusterName", clusterName))
		return nil, ErrClusterAlreadyExists
	}

	clusterID, err := m.allocClusterID(ctx)
	if err != nil {
		log.Error("fail to alloc cluster id", zap.Error(err))
		return nil, errors.Wrapf(err, "clusters manager CreateCluster, clusterName:%s", clusterName)
	}

	clusterPb := &clusterpb.Cluster{
		Id:                clusterID,
		Name:              clusterName,
		MinNodeCount:      initialNodeCount,
		ReplicationFactor: replicationFactor,
		ShardTotal:        shardTotal,
	}
	clusterPb, err = m.storage.CreateCluster(ctx, clusterPb)
	if err != nil {
		log.Error("fail to create cluster", zap.Error(err))
		return nil, errors.Wrapf(err, "clusters manager CreateCluster, clusters:%v", clusterPb)
	}

	cluster := NewCluster(clusterPb, m.storage, m.hbstreams, m.rootPath)

	if err = cluster.init(ctx); err != nil {
		log.Error("fail to init cluster", zap.Error(err))
		return nil, errors.Wrapf(err, "clusters manager CreateCluster, clusterName:%s", clusterName)
	}

	if err := cluster.Load(ctx); err != nil {
		log.Error("fail to load cluster", zap.Error(err))
		return nil, errors.Wrapf(err, "clusters manager CreateCluster, clusterName:%s", clusterName)
	}

	m.clusters[clusterName] = cluster

	return cluster, nil
}

func (m *managerImpl) AllocSchemaID(ctx context.Context, clusterName, schemaName string) (uint32, error) {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return 0, errors.Wrap(err, "clusters manager AllocSchemaID")
	}

	// create new schema
	schema, err := cluster.GetOrCreateSchema(ctx, schemaName)
	if err != nil {
		log.Error("fail to create schema", zap.Error(err))
		return 0, errors.Wrapf(err, "clusters manager AllocSchemaID, "+
			"clusterName:%s, schemaName:%s", clusterName, schemaName)
	}
	return schema.GetID(), nil
}

func (m *managerImpl) AllocTableID(ctx context.Context, clusterName, schemaName, tableName, nodeName string) (*Table, error) {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return nil, errors.Wrap(err, "clusters manager AllocTableID")
	}

	table, err := cluster.GetOrCreateTable(ctx, nodeName, schemaName, tableName)
	if err != nil {
		log.Error("fail to create table", zap.Error(err))
		return nil, errors.Wrapf(err, "clusters manager AllocTableID, "+
			"clusterName:%s, schemaName:%s, tableName:%s, nodeName:%s", clusterName, schemaName, tableName, nodeName)
	}
	return table, nil
}

func (m *managerImpl) GetTables(ctx context.Context, clusterName, nodeName string, shardIDs []uint32) (map[uint32]*ShardTables, error) {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return nil, errors.Wrap(err, "clusters manager GetTables")
	}

	shardTablesWithRole, err := cluster.GetTables(ctx, shardIDs, nodeName)
	if err != nil {
		return nil, errors.Wrapf(err, "clusters manager GetTables, "+
			"clusterName:%s, nodeName:%s, shardIDs:%v", clusterName, nodeName, shardIDs)
	}

	ret := make(map[uint32]*ShardTables, len(shardIDs))
	for shardID, shardTables := range shardTablesWithRole {
		tableInfos := make([]*TableInfo, 0, len(shardTables.tables))

		for _, t := range shardTables.tables {
			tableInfos = append(tableInfos, &TableInfo{
				ID: t.meta.GetId(), Name: t.meta.GetName(),
				SchemaID: t.schema.GetId(), SchemaName: t.schema.GetName(),
			})
		}
		ret[shardID] = &ShardTables{ShardRole: shardTables.shardRole, Tables: tableInfos, Version: shardTables.version}
	}
	return ret, nil
}

func (m *managerImpl) DropTable(ctx context.Context, clusterName, schemaName, tableName string, tableID uint64) error {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return errors.Wrap(err, "clusters manager DropTable")
	}

	if err := cluster.DropTable(ctx, schemaName, tableName, tableID); err != nil {
		return errors.Wrapf(err, "clusters manager DropTable, clusterName:%s, schemaName:%s, tableName:%s, tableID:%d",
			clusterName, schemaName, tableName, tableID)
	}

	return nil
}

func (m *managerImpl) RegisterNode(ctx context.Context, clusterName, nodeName string, lease uint32) error {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return errors.Wrap(err, "clusters manager RegisterNode")
	}
	err = cluster.RegisterNode(ctx, nodeName, lease)
	if err != nil {
		return errors.Wrap(err, "clusters manager RegisterNode")
	}

	// TODO: refactor coordinator
	if err := cluster.coordinator.scatterShard(ctx); err != nil {
		return errors.Wrap(err, "RegisterNode")
	}
	return nil
}

func (m *managerImpl) GetShards(ctx context.Context, clusterName, nodeName string) ([]uint32, error) {
	cluster, err := m.getCluster(ctx, clusterName)
	if err != nil {
		log.Error("cluster not found", zap.Error(err))
		return nil, errors.Wrap(err, "clusters manager GetShards")
	}

	shardIDs, err := cluster.GetShardIDs(nodeName)
	if err != nil {
		return nil, errors.Wrap(err, "clusters manager GetShards")
	}
	return shardIDs, nil
}

func (m *managerImpl) getCluster(_ context.Context, clusterName string) (*Cluster, error) {
	m.lock.RLock()
	cluster, ok := m.clusters[clusterName]
	m.lock.RUnlock()
	if !ok {
		return nil, ErrClusterNotFound.WithCausef("clusters manager getCluster, clusterName:%s", clusterName)
	}
	return cluster, nil
}

func (m *managerImpl) allocClusterID(ctx context.Context) (uint32, error) {
	ID, err := m.alloc.Alloc(ctx)
	if err != nil {
		return 0, errors.Wrapf(err, "alloc cluster id failed")
	}
	return uint32(ID), nil
}
