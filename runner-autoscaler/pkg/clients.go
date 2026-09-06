package pkg

import (
	"context"
	"errors"
	"net/http"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/firestore"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"google.golang.org/api/idtoken"
)

// Initialize once, using a startup context, not a request that can be canceled.
// Tests construct Autoscaler without this method and inject local fakes.
func (s *Autoscaler) Initialize(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	s.computeClient, err = compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return err
	}
	s.operationsClient, err = compute.NewZoneOperationsRESTClient(ctx)
	if err != nil {
		return err
	}
	s.taskClient, err = cloudtasks.NewClient(ctx)
	if err != nil {
		return err
	}
	s.secretClient, err = secretmanager.NewClient(ctx)
	if err != nil {
		return err
	}
	f, err := firestore.NewClientWithDatabase(ctx, s.conf.ProjectId, s.conf.StateDatabase)
	if err != nil {
		return err
	}
	s.store = &firestoreStore{f}
	s.tokenValidator, err = idtoken.NewValidator(ctx)
	s.httpClient = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: 100, MaxIdleConnsPerHost: 32,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}}
	return err
}
func (s *Autoscaler) Close() error {
	var errs []error
	if s.computeClient != nil {
		errs = append(errs, s.computeClient.Close())
	}
	if s.operationsClient != nil {
		errs = append(errs, s.operationsClient.Close())
	}
	if s.taskClient != nil {
		errs = append(errs, s.taskClient.Close())
	}
	if s.secretClient != nil {
		errs = append(errs, s.secretClient.Close())
	}
	if s.store != nil {
		errs = append(errs, s.store.Close())
	}
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	return errors.Join(errs...)
}
func (s *Autoscaler) compute(ctx context.Context) (*InstanceClient, func()) {
	if s.computeClient != nil {
		return &InstanceClient{s.computeClient}, func() {}
	}
	c := newComputeClient(ctx)
	return c, func() { _ = c.Close() }
}
func (s *Autoscaler) tasks(ctx context.Context) (*cloudtasks.Client, func()) {
	if s.taskClient != nil {
		return s.taskClient, func() {}
	}
	c := newTaskClient(ctx)
	return c, func() { _ = c.Close() }
}
func (s *Autoscaler) secrets(ctx context.Context) (*secretmanager.Client, func()) {
	if s.secretClient != nil {
		return s.secretClient, func() {}
	}
	c := newSecretAccessClient(ctx)
	return c, func() { _ = c.Close() }
}
func (s *Autoscaler) http() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}
