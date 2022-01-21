package sectorstorage

import (
	"context"
	"math/rand"
	"sync"
	"time"

	xerrors "golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	sealtasks "github.com/filecoin-project/lotus/extern/sector-storage/sealtasks"
	"github.com/filecoin-project/lotus/extern/sector-storage/stores"
	"github.com/filecoin-project/lotus/extern/sector-storage/storiface"
)

type poStScheduler struct {
	lk      sync.RWMutex
	workers map[storiface.WorkerID]*workerHandle
	cond    *sync.Cond

	postType sealtasks.TaskType
}

func newPoStScheduler(t sealtasks.TaskType) *poStScheduler {
	ps := &poStScheduler{
		workers:  map[storiface.WorkerID]*workerHandle{},
		postType: t,
	}
	ps.cond = sync.NewCond(&ps.lk)
	return ps
}

func (ps *poStScheduler) MaybeAddWorker(wid storiface.WorkerID, tasks map[sealtasks.TaskType]struct{}, w *workerHandle) bool {
	if _, ok := tasks[ps.postType]; !ok {
		return false
	}

	ps.lk.Lock()
	defer ps.lk.Unlock()

	ps.workers[wid] = w

	go ps.watch(wid, w)

	ps.cond.Broadcast()

	return true
}

func (ps *poStScheduler) delWorker(wid storiface.WorkerID) *workerHandle {
	ps.lk.Lock()
	defer ps.lk.Unlock()
	var w *workerHandle = nil
	if wh, ok := ps.workers[wid]; ok {
		w = wh
		delete(ps.workers, wid)
	}
	return w
}

func (ps *poStScheduler) CanSched(ctx context.Context) bool {
	ps.lk.RLock()
	defer ps.lk.RUnlock()
	if len(ps.workers) == 0 {
		return false
	}

	for _, w := range ps.workers {
		if w.enabled {
			return true
		}
	}

	return false
}

func (ps *poStScheduler) Schedule(ctx context.Context, primary bool, spt abi.RegisteredSealProof, work WorkerAction) error {
	ps.lk.Lock()
	defer ps.lk.Unlock()

	if len(ps.workers) == 0 {
		return xerrors.Errorf("can't find %s post worker", ps.postType)
	}

	// Get workers by resource
	canDo, candidates := ps.readyWorkers(spt)
	for !canDo {
		//if primary is true, it must be dispatched to a worker
		if primary {
			ps.cond.Wait()
			canDo, candidates = ps.readyWorkers(spt)
		} else {
			return xerrors.Errorf("can't find %s post worker", ps.postType)
		}
	}

	defer func() {
		if ps.cond != nil {
			ps.cond.Broadcast()
		}
	}()

	selected := candidates[0]
	worker := ps.workers[selected.id]

	return worker.active.withResources(selected.id, worker.info, selected.res, &ps.lk, func() error {
		ps.lk.Unlock()
		defer ps.lk.Lock()

		return work(ctx, worker.workerRpc)
	})
}

type candidateWorker struct {
	id  storiface.WorkerID
	res storiface.Resources
}

func (ps *poStScheduler) readyWorkers(spt abi.RegisteredSealProof) (bool, []candidateWorker) {
	var accepts []candidateWorker
	//if the gpus of the worker are insufficient or it's disabled, it cannot be scheduled
	for wid, wr := range ps.workers {
		needRes := wr.info.Resources.ResourceSpec(spt, ps.postType)

		if !wr.active.canHandleRequest(needRes, wid, "post-readyWorkers", wr.info) {
			continue
		}

		accepts = append(accepts, candidateWorker{
			id:  wid,
			res: needRes,
		})
	}

	// todo: round robin or something
	rand.Shuffle(len(accepts), func(i, j int) {
		accepts[i], accepts[j] = accepts[j], accepts[i]
	})

	return len(accepts) != 0, accepts
}

func (ps *poStScheduler) disable(wid storiface.WorkerID) {
	ps.lk.Lock()
	defer ps.lk.Unlock()
	ps.workers[wid].enabled = false
}

func (ps *poStScheduler) enable(wid storiface.WorkerID) {
	ps.lk.Lock()
	defer ps.lk.Unlock()
	ps.workers[wid].enabled = true
}

func (ps *poStScheduler) watch(wid storiface.WorkerID, worker *workerHandle) {
	heartbeatTimer := time.NewTicker(stores.HeartbeatInterval)
	defer heartbeatTimer.Stop()

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	defer close(worker.closedMgr)

	defer func() {
		log.Warnw("Worker closing", "WorkerID", wid)
		ps.delWorker(wid)
	}()

	for {
		sctx, scancel := context.WithTimeout(ctx, stores.HeartbeatInterval/2)
		curSes, err := worker.workerRpc.Session(sctx)
		scancel()
		if err != nil {
			// Likely temporary error
			log.Warnw("failed to check worker session", "error", err)
			ps.disable(wid)

			select {
			case <-heartbeatTimer.C:
				continue
			case <-worker.closingMgr:
				return
			}
		}

		if storiface.WorkerID(curSes) != wid {
			if curSes != ClosedWorkerID {
				// worker restarted
				log.Warnw("worker session changed (worker restarted?)", "initial", wid, "current", curSes)
			}
			return
		}

		ps.enable(wid)
	}
}

func (ps *poStScheduler) workerCleanup(wid storiface.WorkerID, w *workerHandle) {
	select {
	case <-w.closingMgr:
	default:
		close(w.closingMgr)
	}

	ps.lk.Unlock()
	select {
	case <-w.closedMgr:
	case <-time.After(time.Second):
		log.Errorf("timeout closing worker manager goroutine %s", wid)
	}
	ps.lk.Lock()
}

func (ps *poStScheduler) schedClose() {
	ps.lk.Lock()
	defer ps.lk.Unlock()
	log.Debugf("closing scheduler")

	for i, w := range ps.workers {
		ps.workerCleanup(i, w)
	}
}

func (ps *poStScheduler) WorkerStats(cb func(wid storiface.WorkerID, worker *workerHandle)) {
	ps.lk.RLock()
	defer ps.lk.RUnlock()
	for id, w := range ps.workers {
		cb(id, w)
	}
}
