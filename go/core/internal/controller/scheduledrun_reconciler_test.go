package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

type scheduleProjectionRecorder struct {
	projected *database.ScheduledRun
	disabled  bool
	retired   bool
	failure   error
}

func (s *scheduleProjectionRecorder) SyncScheduledRun(_ context.Context, run *database.ScheduledRun) (*database.ScheduledRun, error) {
	s.projected = run
	return run, s.failure
}
func (s *scheduleProjectionRecorder) GetActiveScheduledRunID(context.Context, types.NamespacedName) (uuid.UUID, error) {
	if s.projected == nil || s.retired {
		return uuid.Nil, database.ErrNotFound
	}
	return uuid.Parse(s.projected.ID)
}
func (s *scheduleProjectionRecorder) DisableScheduledRun(context.Context, uuid.UUID, int64) error {
	s.disabled = true
	return s.failure
}
func (s *scheduleProjectionRecorder) RetireScheduledRun(context.Context, uuid.UUID) error {
	s.retired = true
	return s.failure
}

func TestScheduledRunReconciliationLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		opts := krt.NewOptionsBuilder(ctx.Done(), "schedule-test", nil)
		resource := &v1alpha3.ScheduledRun{ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "team", UID: types.UID(uuid.NewString()), Generation: 1}, Spec: v1alpha3.ScheduledRunSpec{AgentRef: corev1.LocalObjectReference{Name: "agent"}, Schedule: "0 9 * * *", Prompt: "hello"}}
		agent := &v1alpha3.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "team", UID: types.UID(uuid.NewString())}}
		resources := krt.NewStaticCollection(nil, []*v1alpha3.ScheduledRun{resource}, opts.WithName("resources")...)
		agents := krt.NewStaticCollection(nil, []*v1alpha3.Agent{agent}, opts.WithName("agents")...)
		states := newScheduledRunReconciliations(resources, agents, opts)
		synctest.Wait()
		scheme := runtime.NewScheme()
		require.NoError(t, v1alpha3.AddToScheme(scheme))
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha3.ScheduledRun{}).WithObjects(resource).Build()
		store := &scheduleProjectionRecorder{}
		reconciler := &ScheduledRunReconciler{states: states, store: store, kube: kube, reader: kube, caller: auth.NewCredentialSession(auth.Principal{ID: "scheduler"}, nil)}
		key := client.ObjectKeyFromObject(resource)
		refresh := func() {
			require.NoError(t, kube.Get(ctx, key, resource))
			resources.UpdateObject(resource.DeepCopy())
			synctest.Wait()
		}
		// No executable projection can precede the durable deletion fence.
		require.NoError(t, reconciler.reconcile(ctx, "team/daily"))
		require.Nil(t, store.projected)
		refresh()
		require.Contains(t, resource.Finalizers, v1alpha3.ScheduledRunFinalizer)
		require.NoError(t, reconciler.reconcile(ctx, "team/daily"))
		refresh()
		require.Equal(t, "scheduler", store.projected.Creator)
		require.Equal(t, string(agent.UID), store.projected.AgentUID)
		require.True(t, meta.IsStatusConditionTrue(resource.Status.Conditions, v1alpha3.ScheduledRunConditionReady))
		// Agent deletion disables new occurrences, and recreation records its new UID.
		agents.DeleteObject("team/agent")
		synctest.Wait()
		require.NoError(t, reconciler.reconcile(ctx, "team/daily"))
		refresh()
		require.True(t, store.disabled)
		require.True(t, meta.IsStatusConditionFalse(resource.Status.Conditions, v1alpha3.ScheduledRunConditionReady))
		agent.UID = types.UID(uuid.NewString())
		agents.UpdateObject(agent.DeepCopy())
		synctest.Wait()
		require.NoError(t, reconciler.reconcile(ctx, "team/daily"))
		refresh()
		require.Equal(t, string(agent.UID), store.projected.AgentUID)
		// A failed retirement must leave the finalizer intact for retry.
		require.NoError(t, kube.Delete(ctx, resource))
		refresh()
		store.failure = errors.New("database unavailable")
		require.Error(t, reconciler.reconcile(ctx, "team/daily"))
		refresh()
		require.Contains(t, resource.Finalizers, v1alpha3.ScheduledRunFinalizer)
		store.failure = nil
		require.NoError(t, reconciler.reconcile(ctx, "team/daily"))
		require.True(t, store.retired)
		require.Error(t, kube.Get(ctx, key, &v1alpha3.ScheduledRun{}))
	})
}

func TestScheduledRunDeleteEventRetiresProjectionWithoutFinalizer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		resource := &v1alpha3.ScheduledRun{ObjectMeta: metav1.ObjectMeta{
			Name: "daily", Namespace: "team", UID: types.UID(uuid.NewString()), Generation: 1,
			Finalizers: []string{v1alpha3.ScheduledRunFinalizer},
		}, Spec: v1alpha3.ScheduledRunSpec{AgentRef: corev1.LocalObjectReference{Name: "agent"}, Schedule: "0 9 * * *", Prompt: "hello"}}
		opts := krt.NewOptionsBuilder(ctx.Done(), "delete-event", nil)
		states := krt.NewStaticCollection(nil, []ScheduledRunReconciliation{{Resource: resource, AgentUID: "agent-uid"}}, opts.WithName("states")...)
		scheme := runtime.NewScheme()
		require.NoError(t, v1alpha3.AddToScheme(scheme))
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha3.ScheduledRun{}).WithObjects(resource).Build()
		store := &scheduleProjectionRecorder{}
		reconciler := NewScheduledRunReconciler(states, store, kube, kube, auth.NewCredentialSession(auth.Principal{ID: "scheduler"}, nil))
		done := make(chan error, 1)
		go func() { done <- reconciler.Start(ctx) }()
		synctest.Wait()
		require.NotNil(t, store.projected)
		require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(resource), resource))
		resource.Finalizers = nil
		require.NoError(t, kube.Update(ctx, resource))
		require.NoError(t, kube.Delete(ctx, resource))
		states.DeleteObject("team/daily")
		synctest.Wait()
		require.True(t, store.retired, "a delete event must retire the durable projection even when the finalizer was removed")
		cancel()
		require.NoError(t, <-done)
	})
}
