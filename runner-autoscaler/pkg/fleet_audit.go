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

func (s *Autoscaler) reconcileRunners(ctx context.Context) error {
	if s.conf.Simulate {
		return nil
	}
	rows, err := s.store.RunnerPage(ctx, time.Now())
	if err != nil {
		return err
	}
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
		if found && !remove {
			src, ok := s.conf.RegisteredSources[r.Record.Source]
			if !ok {
				return fmt.Errorf("unknown source for detached runner %s", r.Name)
			}
			registrationFn := s.runnerStateFn
			if registrationFn == nil {
				registrationFn = s.runnerState
			}
			registration, err := registrationFn(ctx, src, r.Name)
			if err != nil {
				return err
			}
			remove = registration == runnerGone || (r.Record.Terminal && registration == runnerIdle)
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
			if e = s.store.DeferRunner(ctx, r.Name, time.Now().Add(10*time.Minute)); e != nil {
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
	client, closeClient := s.compute(ctx)
	defer closeClient()
	for _, zone := range s.conf.Zones {
		it := client.List(ctx, &computepb.ListInstancesRequest{Project: s.conf.ProjectId, Zone: zone, Filter: proto.String(fmt.Sprintf("name eq ^%s-.*", s.conf.RunnerPrefix))})
		for {
			vm, e := it.Next()
			if e == iterator.Done {
				break
			}
			if e != nil {
				c.AbortWithError(503, e)
				return
			}
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
