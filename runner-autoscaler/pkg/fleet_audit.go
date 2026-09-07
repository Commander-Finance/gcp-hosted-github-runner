package pkg

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

// Every processed runner leaves the due set (deferred or released), so
// re-reading the page with the same cutoff yields the next batch. The page
// bound keeps one sweep within its deadline; later sweeps drain the remainder.
const maxRunnerPagesPerSweep = 10

func (s *Autoscaler) reconcileRunners(ctx context.Context) error {
	if s.conf.Simulate {
		return nil
	}
	due := time.Now()
	for page := 0; page < maxRunnerPagesPerSweep; page++ {
		rows, err := s.store.RunnerPage(ctx, due)
		if err != nil {
			return err
		}
		if err = s.reconcileRunnerRows(ctx, rows); err != nil {
			return err
		}
		if len(rows) < runnerPageSize {
			return nil
		}
	}
	return nil
}
func (s *Autoscaler) reconcileRunnerRows(ctx context.Context, rows []runnerRecord) error {
	for _, r := range rows {
		stateFn := s.instanceStateFn
		if stateFn == nil {
			stateFn = s.instanceState
		}
		found, state, e := stateFn(ctx, r.Name)
		if e != nil {
			return e
		}
		remove := found && state.isStopped()
		available := false
		next := time.Now().Add(10 * time.Minute)
		if found && !remove {
			src, ok := s.conf.RegisteredSources[r.Record.Source]
			if !ok {
				// A permanent condition, not a transient error: failing the sweep
				// would re-sort this record to the front of every later page and
				// stall reclamation fleet-wide. Defer it and keep sweeping.
				log.Errorf("Lifecycle sweep skipped detached runner %s: unknown source %s", r.Name, r.Record.Source)
				if e = s.store.DeferRunner(ctx, r.Name, next, false); e != nil {
					return e
				}
				continue
			}
			registration, err := s.capacityRegistration(ctx, src, r.Name)
			if err != nil {
				return err
			}
			remove = registration == runnerGone || (r.Record.Terminal && registration != runnerBusy) || (registration == runnerOffline && s.offlineExpired(r.Record))
			available = registration == runnerIdle
			if registration == runnerOffline {
				next = time.Now().Add(2 * time.Minute)
			}
		}
		if remove {
			if e = s.DeleteInstance(ctx, r.Name); e != nil {
				return e
			}
			found = false
		}
		if !found {
			if e = s.store.ReleaseRunner(ctx, r.Name); e != nil {
				return e
			}
		} else {
			if e = s.store.DeferRunner(ctx, r.Name, next, available); e != nil {
				return e
			}
		}
	}
	return nil
}
func ledgerDiscrepancy(counts fleetState, rows []runnerRecord, actual map[string]string) error {
	expected := map[string]runnerRecord{}
	standard := 0
	for _, r := range rows {
		expected[r.Name] = r
		if r.Record.Model == "standard" {
			standard++
		}
	}
	if counts.Runners != len(rows) || counts.Standard != standard {
		return fmt.Errorf("reservation counters differ from ledger: counters=%d/%d ledger=%d/%d", counts.Runners, counts.Standard, len(rows), standard)
	}
	for name, model := range actual {
		r, ok := expected[name]
		if !ok {
			return fmt.Errorf("GCE runner %s has no durable reservation", name)
		}
		if r.Record.Model != "" && r.Record.Model != model {
			return fmt.Errorf("GCE runner %s model differs from reservation", name)
		}
	}
	for name, r := range expected {
		if _, ok := actual[name]; !ok && !r.Record.CreatedAt.IsZero() && time.Since(r.Record.CreatedAt) > 5*time.Minute {
			return fmt.Errorf("confirmed runner %s missing from GCE; reservation requires reconciliation", name)
		}
	}
	return nil
}

// listInstances lists the runner-prefixed GCE instances in a zone, using
// listInstancesFn when set (tests) or the real compute client otherwise.
func (s *Autoscaler) listInstances(ctx context.Context, zone string) ([]*computepb.Instance, error) {
	if s.listInstancesFn != nil {
		return s.listInstancesFn(ctx, zone)
	}
	client, closeClient := s.compute(ctx)
	defer closeClient()
	it := client.List(ctx, &computepb.ListInstancesRequest{Project: s.conf.ProjectId, Zone: zone, Filter: proto.String(fmt.Sprintf("name eq ^%s-.*", s.conf.RunnerPrefix))})
	var instances []*computepb.Instance
	for {
		vm, e := it.Next()
		if e == iterator.Done {
			break
		}
		if e != nil {
			return nil, e
		}
		instances = append(instances, vm)
	}
	return instances, nil
}
func (s *Autoscaler) auditFleet(c *gin.Context) {
	if !s.privateRequest(c) {
		return
	}
	ctx, cancel := s.opContext()
	defer cancel()
	counts, rows, err := s.store.AuditSnapshot(ctx)
	if err != nil {
		log.Errorf("Lifecycle invariant failed: %v", err)
		c.AbortWithError(503, err)
		return
	}
	actual := map[string]string{}
	for _, zone := range s.conf.Zones {
		instances, e := s.listInstances(ctx, zone)
		if e != nil {
			c.AbortWithError(503, e)
			return
		}
		for _, vm := range instances {
			model := "standard"
			if vm.GetScheduling().GetProvisioningModel() == "SPOT" || vm.GetScheduling().GetPreemptible() {
				model = "spot"
			}
			actual[vm.GetName()] = model
		}
	}
	after, _, err := s.store.AuditSnapshot(ctx)
	if err != nil {
		c.AbortWithError(503, err)
		return
	}
	if after.Revision != counts.Revision {
		log.Info("Fleet audit deferred: capacity changed during GCE inventory")
		c.Status(200)
		return
	}
	if err = ledgerDiscrepancy(counts, rows, actual); err != nil {
		log.Errorf("Lifecycle invariant failed: %v", err)
	} else {
		log.WithField("reservations", len(rows)).Info("Fleet invariant audit passed")
	}
	// Discrepancies alert an operator. Never rewrite counters from a moving GCE
	// inventory: pending inserts are legitimate reservations even without a VM.
	c.Status(200)
}
