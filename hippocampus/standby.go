package hippocampus

import (
	"time"

	log "github.com/sirupsen/logrus"
)

// Consolidator failover (TODO-3 item 169).
//
// A shared store has exactly one consolidating instance, enforced by the single-consolidator lock,
// and a store with none never forgets: every instance stays healthy while the store grows past its
// capacity target, which is the failure the topology view's zero-consolidators warning exists to
// name. Taking over used to be manual. A standby (consolidation.standby) starts as a replica and
// polls the lock; when it wins - immediately, or once the consolidator's session has ended - it
// promotes itself and starts exactly the work New starts for a consolidator.
//
// Run EVERY instance as a standby and the first to poll after a consolidator dies takes over, with
// no instance configured as the leader; an instance configured with consolidation.enabled true and
// restarted after a standby took over would instead find the lock held and fail to start.

// defaultStandbyPoll is how often a standby asks for the lock when consolidation.standbyPollSeconds
// is unset. A consolidator's session ends with its process, so this is most of the failover time.
const defaultStandbyPoll = 15 * time.Second

// instanceLocker is the seam onto the concrete store's lock, which the Store interface does not
// carry - SetSearchOutbox's precedent.
type instanceLocker interface {
	TryAcquireInstanceLock() (bool, error)
}

// consolidating reports whether this instance runs the sleep cycle: configured to, or a standby that
// has won the lock.
func (s *Server) consolidating() bool {
	return s.consolidationEnabled || s.promoted.Load()
}

// runsConsolidatorWork is the gate the consolidator-only workers start behind. It is consolidating()
// plus a promotion in progress: promote starts the workers BEFORE it lets the rest of the server see
// the new role, so an RPC that reads consolidating() as true can rely on everything those workers
// set up already being in place - the atomic store that flips the role is what publishes it.
func (s *Server) runsConsolidatorWork() bool {
	return s.consolidating() || s.promoting.Load()
}

// startStandby starts the poll when this instance is a standby.
func (s *Server) startStandby(deps Dependencies) {
	if !s.standby || s.consolidationEnabled {
		return
	}

	locker, ok := s.db.(instanceLocker)
	if !ok {
		log.Warn("consolidation.standby is set but this store has no shared lock to wait for; the instance stays a replica")

		return
	}

	poll := s.standbyPoll
	if poll <= 0 {
		poll = defaultStandbyPoll
	}

	s.standbyDeps = deps
	s.stopStandby = make(chan struct{})
	s.standbyStopped = make(chan struct{})

	log.Infof("consolidation.standby: serving as a replica and asking for the consolidator lock every %s", poll)

	go func() {
		defer close(s.standbyStopped)

		ticker := time.NewTicker(poll)
		defer ticker.Stop()

		for {
			if s.tryPromote(locker) {
				return
			}

			select {

			case <-s.stopStandby:
				return

			case <-ticker.C:

			}
		}
	}()
}

// tryPromote asks for the lock once, and promotes on winning it.
func (s *Server) tryPromote(locker instanceLocker) bool {
	won, err := locker.TryAcquireInstanceLock()
	if err != nil {
		log.Warnf("consolidation.standby: could not ask for the consolidator lock, will ask again: %s", err.Error())

		return false
	}

	if !won {
		return false
	}

	return s.promote()
}

// promote turns this replica into the consolidator: the sleep loop restarts with the real period,
// and the workers only a consolidator runs start. Returns false when Stop got there first.
func (s *Server) promote() bool {
	s.promoteMu.Lock()
	defer s.promoteMu.Unlock()

	s.cycleMu.Lock()
	stopping := s.cyclesStopping
	s.cycleMu.Unlock()

	if stopping {
		return false
	}

	s.promoting.Store(true)
	defer s.promoting.Store(false)

	log.Warn("consolidation.standby: won the consolidator lock - this instance now runs the sleep cycle for the shared store")

	s.logForgettingMode()

	// The replica's sleep loop runs with no period; replace it with one that has the real one.
	if s.stopSleep != nil {
		close(s.stopSleep)
		<-s.sleepStopped
	}

	s.stopSleep = make(chan struct{})
	s.sleepStopped = make(chan struct{})
	s.autoSleep(s.sleepReset, s.sleepPeriod)

	s.startReconcile(s.standbyDeps.Search)
	s.startScheduledExport()
	s.startOutboxDrain(s.standbyDeps.Search)
	s.startCallbackDispatch(s.standbyDeps.Notifier)

	// Last, and only now: see runsConsolidatorWork.
	s.promoted.Store(true)

	return true
}

// stopStandbyLoop stops the poll, if one is running, and waits for it.
func (s *Server) stopStandbyLoop() {
	if s.stopStandby == nil {
		return
	}

	close(s.stopStandby)
	<-s.standbyStopped
}
