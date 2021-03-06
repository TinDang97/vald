//
// Copyright (C) 2019-2020 Vdaas.org Vald team ( kpango, rinx, kmrmt )
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Package service manages the main logic of server.
package service

import (
	"context"
	"encoding/gob"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vdaas/vald/internal/config"
	core "github.com/vdaas/vald/internal/core/ngt"
	"github.com/vdaas/vald/internal/errgroup"
	"github.com/vdaas/vald/internal/errors"
	"github.com/vdaas/vald/internal/file"
	"github.com/vdaas/vald/internal/log"
	"github.com/vdaas/vald/internal/observability/trace"
	"github.com/vdaas/vald/internal/rand"
	"github.com/vdaas/vald/internal/safety"
	"github.com/vdaas/vald/internal/timeutil"
	"github.com/vdaas/vald/pkg/agent/core/ngt/model"
	"github.com/vdaas/vald/pkg/agent/core/ngt/service/kvs"
)

type NGT interface {
	Start(ctx context.Context) <-chan error
	Search(vec []float32, size uint32, epsilon, radius float32) ([]model.Distance, error)
	SearchByID(uuid string, size uint32, epsilon, radius float32) ([]model.Distance, error)
	Insert(uuid string, vec []float32) (err error)
	InsertMultiple(vecs map[string][]float32) (err error)
	Update(uuid string, vec []float32) (err error)
	UpdateMultiple(vecs map[string][]float32) (err error)
	Delete(uuid string) (err error)
	DeleteMultiple(uuids ...string) (err error)
	GetObject(uuid string) (vec []float32, err error)
	CreateIndex(ctx context.Context, poolSize uint32) (err error)
	SaveIndex(ctx context.Context) (err error)
	Exists(string) (uint32, bool)
	CreateAndSaveIndex(ctx context.Context, poolSize uint32) (err error)
	IsIndexing() bool
	Len() uint64
	NumberOfCreateIndexExecution() uint64
	UUIDs(context.Context) (uuids []string)
	UncommittedUUIDs() (uuids []string)
	DeleteVCacheLen() uint64
	InsertVCacheLen() uint64
	Close(ctx context.Context) error
}

type ngt struct {
	alen     int
	indexing atomic.Value
	lim      time.Duration // auto indexing time limit
	dur      time.Duration // auto indexing check duration
	sdur     time.Duration // auto save index check duration
	idelay   time.Duration // initial delay duration
	dps      uint32        // default pool size
	ic       uint64        // insert count
	nocie    uint64        // number of create index execution
	eg       errgroup.Group
	ivc      *vcaches // insertion vector cache
	dvc      *vcaches // deletion vector cache
	path     string
	kvs      kvs.BidiMap
	core     core.NGT
	dcd      bool // disable commit daemon
	inMem    bool
}

type vcache struct {
	vector []float32
	date   int64
}

const (
	kvsFileName = "ngt-meta.kvsdb"
)

func New(cfg *config.NGT) (nn NGT, err error) {
	n := new(ngt)
	n.inMem = cfg.EnableInMemoryMode
	cfg.IndexPath = strings.TrimSuffix(cfg.IndexPath, "/")
	opts := []core.Option{
		core.WithInMemoryMode(n.inMem),
		core.WithIndexPath(cfg.IndexPath),
		core.WithDimension(cfg.Dimension),
		core.WithDistanceTypeByString(cfg.DistanceType),
		core.WithObjectTypeByString(cfg.ObjectType),
		core.WithBulkInsertChunkSize(cfg.BulkInsertChunkSize),
		core.WithCreationEdgeSize(cfg.CreationEdgeSize),
		core.WithSearchEdgeSize(cfg.SearchEdgeSize),
	}

	if !n.inMem && len(cfg.IndexPath) != 0 {
		n.path = cfg.IndexPath
	}

	n.kvs = kvs.New()

	if _, err = os.Stat(cfg.IndexPath); os.IsNotExist(err) || n.inMem {
		n.core, err = core.New(opts...)
	} else {
		eg, _ := errgroup.New(context.Background())
		eg.Go(safety.RecoverFunc(func() (err error) {
			n.core, err = core.Load(opts...)
			return err
		}))
		eg.Go(safety.RecoverFunc(func() (err error) {
			if len(n.path) != 0 && !n.inMem {
				m := make(map[string]uint32)
				gob.Register(map[string]uint32{})
				f := file.Open(n.path+"/"+kvsFileName, os.O_RDONLY|os.O_SYNC, os.ModePerm)
				defer f.Close()
				err = gob.NewDecoder(f).Decode(&m)
				if err != nil {
					return err
				}
				for k, id := range m {
					n.kvs.Set(k, id)
				}
			}
			return nil
		}))
		err = eg.Wait()
	}
	if err != nil {
		return nil, err
	}

	if cfg.AutoIndexCheckDuration != "" {
		d, err := timeutil.Parse(cfg.AutoIndexCheckDuration)
		if err != nil {
			d = 0
		}
		n.dur = d
	}

	if cfg.AutoIndexDurationLimit != "" {
		d, err := timeutil.Parse(cfg.AutoIndexDurationLimit)
		if err != nil {
			d = 0
		}
		n.lim = d
	}

	if cfg.AutoSaveIndexDuration != "" {
		d, err := timeutil.Parse(cfg.AutoSaveIndexDuration)
		if err != nil {
			d = 0
		}
		n.sdur = d
	}

	if cfg.InitialDelayMaxDuration != "" {
		d, err := timeutil.Parse(cfg.InitialDelayMaxDuration)
		if err != nil {
			d = 0
		}
		n.idelay = time.Duration(
			int64(rand.LimitedUint32(uint64(d/time.Second))),
		) * time.Second
	}

	n.alen = cfg.AutoIndexLength

	n.eg = errgroup.Get()

	if n.dur == 0 || n.alen == 0 {
		n.dcd = true
	}
	if n.ivc == nil {
		n.ivc = new(vcaches)
	}
	if n.dvc == nil {
		n.dvc = new(vcaches)
	}

	if in, ok := n.indexing.Load().(bool); !ok || in {
		n.indexing.Store(false)
	}

	return n, nil
}

func (n *ngt) Start(ctx context.Context) <-chan error {
	if n.dcd {
		return nil
	}
	ech := make(chan error, 2)
	n.eg.Go(safety.RecoverFunc(func() (err error) {
		if n.sdur == 0 {
			n.sdur = n.dur + time.Second
		}
		if n.lim == 0 {
			n.lim = n.dur * 2
		}
		defer close(ech)

		timer := time.NewTimer(n.idelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		timer.Stop()

		tick := time.NewTicker(n.dur)
		sTick := time.NewTicker(n.sdur)
		limit := time.NewTicker(n.lim)
		defer tick.Stop()
		defer sTick.Stop()
		defer limit.Stop()
		for {
			err = nil
			select {
			case <-ctx.Done():
				err = n.CreateAndSaveIndex(ctx, n.dps)
				if err != nil {
					ech <- err
					return errors.Wrap(ctx.Err(), err.Error())
				}
				return ctx.Err()
			case <-tick.C:
				if int(atomic.LoadUint64(&n.ic)) >= n.alen {
					err = n.CreateIndex(ctx, n.dps)
				}
			case <-limit.C:
				err = n.CreateAndSaveIndex(ctx, n.dps)
			case <-sTick.C:
				err = n.SaveIndex(ctx)
			}
			if err != nil && err != errors.ErrUncommittedIndexNotFound {
				ech <- err
				runtime.Gosched()
				err = nil
			}
		}
	}))

	return ech
}

func (n *ngt) Search(vec []float32, size uint32, epsilon, radius float32) ([]model.Distance, error) {
	if n.indexing.Load().(bool) {
		return make([]model.Distance, 0), nil
	}
	sr, err := n.core.Search(vec, int(size), epsilon, radius)
	if err != nil {
		return nil, err
	}

	ds := make([]model.Distance, 0, len(sr))
	for _, d := range sr {
		if err = d.Error; d.ID == 0 && err != nil {
			log.Debug(err)
			continue
		}
		key, ok := n.kvs.GetInverse(d.ID)
		if ok {
			ds = append(ds, model.Distance{
				ID:       key,
				Distance: d.Distance,
			})
		}
	}

	return ds, nil
}

func (n *ngt) SearchByID(uuid string, size uint32, epsilon, radius float32) (dst []model.Distance, err error) {
	if n.indexing.Load().(bool) {
		log.Debug("SearchByID\t now indexing...")
		return make([]model.Distance, 0), nil
	}
	log.Debugf("SearchByID\tuuid: %s size: %d epsilon: %f radius: %f", uuid, size, epsilon, radius)
	vec, err := n.GetObject(uuid)
	if err != nil {
		log.Debugf("SearchByID\tuuid: %s's vector not found", uuid)
		return nil, err
	}
	return n.Search(vec, size, epsilon, radius)
}

func (n *ngt) Insert(uuid string, vec []float32) (err error) {
	return n.insert(uuid, vec, time.Now().UnixNano(), true)
}

func (n *ngt) insert(uuid string, vec []float32, t int64, validation bool) (err error) {
	if len(uuid) == 0 {
		err = errors.ErrUUIDNotFound(0)
		return err
	}
	if validation {
		id, ok := n.kvs.Get(uuid)
		if ok {
			err = errors.ErrUUIDAlreadyExists(uuid, uint(id))
			return err
		}
		_, ok = n.insertCache(uuid)
		if ok {
			err = errors.ErrUUIDAlreadyExists(uuid, uint(id))
			return err
		}
	}
	n.ivc.Store(uuid, vcache{
		vector: vec,
		date:   t,
	})
	atomic.AddUint64(&n.ic, 1)
	return nil
}

func (n *ngt) InsertMultiple(vecs map[string][]float32) (err error) {
	t := time.Now().UnixNano()
	for uuid, vec := range vecs {
		ierr := n.insert(uuid, vec, t, true)
		if ierr != nil {
			if err != nil {
				err = errors.Wrap(ierr, err.Error())
			} else {
				err = ierr
			}
		}
	}
	return err
}

func (n *ngt) Update(uuid string, vec []float32) (err error) {
	now := time.Now().UnixNano()
	err = n.delete(uuid, now)
	if err != nil {
		return err
	}
	now++
	return n.insert(uuid, vec, now, false)
}

func (n *ngt) UpdateMultiple(vecs map[string][]float32) (err error) {
	uuids := make([]string, 0, len(vecs))
	for uuid := range vecs {
		uuids = append(uuids, uuid)
	}
	err = n.DeleteMultiple(uuids...)
	if err != nil {
		for _, uuid := range uuids {
			n.dvc.Delete(uuid)
		}
		return err
	}
	t := time.Now().UnixNano()
	for uuid, vec := range vecs {
		ierr := n.insert(uuid, vec, t, false)
		if ierr != nil {
			n.dvc.Delete(uuid)
			n.ivc.Delete(uuid)
			atomic.AddUint64(&n.ic, ^uint64(0))
			if err != nil {
				err = errors.Wrap(ierr, err.Error())
			} else {
				err = ierr
			}
		}
	}
	return err
}

func (n *ngt) Delete(uuid string) (err error) {
	return n.delete(uuid, time.Now().UnixNano())
}

func (n *ngt) delete(uuid string, t int64) (err error) {
	if len(uuid) == 0 {
		err = errors.ErrUUIDNotFound(0)
		return err
	}
	_, ok := n.kvs.Get(uuid)
	if !ok {
		_, ok := n.insertCache(uuid)
		if !ok {
			err = errors.ErrObjectIDNotFound(uuid)
			return err
		}
	}
	if vc, ok := n.ivc.Load(uuid); ok && vc.date < t {
		n.ivc.Delete(uuid)
		atomic.AddUint64(&n.ic, ^uint64(0))
	}
	n.dvc.Store(uuid, vcache{
		date: t,
	})
	return nil
}

func (n *ngt) DeleteMultiple(uuids ...string) (err error) {
	t := time.Now().UnixNano()
	for _, uuid := range uuids {
		ierr := n.delete(uuid, t)
		if ierr != nil {
			if err != nil {
				err = errors.Wrap(ierr, err.Error())
			} else {
				err = ierr
			}
		}
	}
	return err
}

func (n *ngt) GetObject(uuid string) (vec []float32, err error) {
	oid, ok := n.kvs.Get(uuid)
	if !ok {
		log.Debugf("GetObject\tuuid: %s's kvs data not found, trying to read from vcache", uuid)
		ivc, ok := n.insertCache(uuid)
		if !ok {
			log.Debugf("GetObject\tuuid: %s's vcache data not found", uuid)
			return nil, errors.ErrObjectIDNotFound(uuid)
		}
		return ivc.vector, nil
	}
	log.Debugf("GetObject\tGetVector oid: %d", oid)
	vec, err = n.core.GetVector(uint(oid))
	if err != nil {
		log.Debugf("GetObject\tuuid: %s oid: %d's vector not found", uuid, oid)
		return nil, errors.ErrObjectNotFound(err, uuid)
	}
	return vec, nil
}

func (n *ngt) CreateIndex(ctx context.Context, poolSize uint32) (err error) {
	ctx, span := trace.StartSpan(ctx, "vald/agent-ngt/service/NGT.CreateIndex")
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	if n.indexing.Load().(bool) {
		return nil
	}
	ic := atomic.LoadUint64(&n.ic)
	if ic == 0 {
		return errors.ErrUncommittedIndexNotFound
	}
	n.indexing.Store(true)
	atomic.StoreUint64(&n.ic, 0)
	t := time.Now().UnixNano()
	defer n.indexing.Store(false)

	log.Infof("create index operation started, uncommitted indexes = %d", ic)
	delList := make([]string, 0, ic)
	n.dvc.Range(func(uuid string, dvc vcache) bool {
		if dvc.date > t {
			return true
		}
		if ivc, ok := n.ivc.Load(uuid); ok && ivc.date < t && ivc.date < dvc.date {
			n.ivc.Delete(uuid)
		}
		delList = append(delList, uuid)
		return true
	})
	log.Info("create index delete kvs phase started")
	log.Debug(delList)
	doids := make([]uint, 0, ic)
	for _, duuid := range delList {
		n.dvc.Delete(duuid)
		id, ok := n.kvs.Delete(duuid)
		if !ok {
			log.Error(errors.ErrObjectIDNotFound(duuid).Error())
			err = errors.Wrap(err, errors.ErrObjectIDNotFound(duuid).Error())
		} else {
			doids = append(doids, uint(id))
		}
	}
	log.Info("create index delete kvs phase finished")

	log.Info("create index delete index phase started")
	log.Debug(doids)
	brerr := n.core.BulkRemove(doids...)
	log.Info("create index delete index phase finished")
	if brerr != nil {
		log.Error(brerr)
		err = errors.Wrap(err, brerr.Error())
	}
	uuids := make([]string, 0, ic)
	vecs := make([][]float32, 0, ic)
	n.ivc.Range(func(uuid string, ivc vcache) bool {
		if ivc.date <= t {
			uuids = append(uuids, uuid)
			vecs = append(vecs, ivc.vector)
		}
		return true
	})
	log.Info("create index insert index phase started")
	log.Debug(vecs)
	oids, errs := n.core.BulkInsert(vecs)
	log.Info("create index insert index phase finished")
	if errs != nil && len(errs) != 0 {
		for _, bierr := range errs {
			if bierr != nil {
				log.Error(bierr)
				err = errors.Wrap(err, bierr.Error())
			}
		}
	}

	log.Info("create index insert kvs phase started")
	log.Debugf("uuids = %#v\t\toids = %#v", uuids, oids)
	for i, uuid := range uuids {
		n.ivc.Delete(uuid)
		if len(oids) > i {
			oid := uint32(oids[i])
			if oid != 0 {
				n.kvs.Set(uuid, oid)
			}
		}
	}
	log.Info("create index insert kvs phase finished")

	log.Info("create graph and tree phase started")
	log.Debugf("pool size = %d", poolSize)
	cierr := n.core.CreateIndex(poolSize)
	if cierr != nil {
		log.Error(cierr)
		err = errors.Wrap(err, cierr.Error())
	}
	log.Info("create graph and tree phase finished")

	log.Info("create index operation finished")
	atomic.AddUint64(&n.nocie, 1)
	return err
}

func (n *ngt) SaveIndex(ctx context.Context) (err error) {
	ctx, span := trace.StartSpan(ctx, "vald/agent-ngt/service/NGT.SaveIndex")
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	if len(n.path) != 0 && !n.inMem {
		eg, ctx := errgroup.New(ctx)
		eg.Go(safety.RecoverFunc(func() error {
			if len(n.path) != 0 {
				m := make(map[string]uint32, n.kvs.Len())
				var mu sync.Mutex
				n.kvs.Range(ctx, func(key string, id uint32) bool {
					mu.Lock()
					m[key] = id
					mu.Unlock()
					return true
				})
				f := file.Open(n.path+"/"+kvsFileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
				defer f.Close()
				gob.Register(map[string]uint32{})
				return gob.NewEncoder(f).Encode(&m)
			}
			return nil
		}))
		eg.Go(safety.RecoverFunc(func() error {
			return n.core.SaveIndex()
		}))
		err = eg.Wait()
	}
	return
}

func (n *ngt) CreateAndSaveIndex(ctx context.Context, poolSize uint32) (err error) {
	ctx, span := trace.StartSpan(ctx, "vald/agent-ngt/service/NGT.CreateAndSaveIndex")
	defer func() {
		if span != nil {
			span.End()
		}
	}()

	err = n.CreateIndex(ctx, poolSize)
	if err != nil {
		return err
	}
	return n.SaveIndex(ctx)
}

func (n *ngt) Exists(uuid string) (oid uint32, ok bool) {
	oid, ok = n.kvs.Get(uuid)
	if !ok {
		_, ok = n.insertCache(uuid)
	}
	return oid, ok
}

func (n *ngt) insertCache(uuid string) (*vcache, bool) {
	iv, ok := n.ivc.Load(uuid)
	if ok {
		dv, ok := n.dvc.Load(uuid)
		if !ok {
			return &iv, true
		}
		if ok && dv.date <= iv.date {
			return &iv, true
		}
		n.ivc.Delete(uuid)
		atomic.AddUint64(&n.ic, ^uint64(0))
	}
	return nil, false
}

func (n *ngt) IsIndexing() bool {
	return n.indexing.Load().(bool)
}

func (n *ngt) UUIDs(ctx context.Context) (uuids []string) {
	uuids = make([]string, 0, n.kvs.Len())
	n.kvs.Range(ctx, func(uuid string, oid uint32) bool {
		uuids = append(uuids, uuid)
		return true
	})
	return uuids
}

func (n *ngt) UncommittedUUIDs() (uuids []string) {
	var mu sync.Mutex
	uuids = make([]string, 0, atomic.LoadUint64(&n.ic))
	n.ivc.Range(func(uuid string, vc vcache) bool {
		mu.Lock()
		uuids = append(uuids, uuid)
		mu.Unlock()
		return true
	})
	return uuids
}

func (n *ngt) NumberOfCreateIndexExecution() uint64 {
	return atomic.LoadUint64(&n.nocie)
}

func (n *ngt) Len() uint64 {
	return n.kvs.Len()
}

func (n *ngt) InsertVCacheLen() uint64 {
	return n.ivc.Len()
}

func (n *ngt) DeleteVCacheLen() uint64 {
	return n.dvc.Len()
}

func (n *ngt) Close(ctx context.Context) (err error) {
	if len(n.path) != 0 {
		err = n.SaveIndex(ctx)
	}
	n.core.Close()
	return
}
