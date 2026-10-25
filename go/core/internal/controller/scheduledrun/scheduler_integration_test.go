package scheduledrun

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/dbtest"
)

func TestSchedulerValidatesKubernetesBeforeReservation(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.StartT(ctx, t)
	dbtest.MigrateT(t, dsn, false)
	db, err := database.Connect(ctx, &database.PostgresConfig{Pool: dbtest.PoolConfig(dsn)})
	require.NoError(t, err)
	t.Cleanup(db.Close)
	store := database.NewClient(db, "public")
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	for i, test := range []struct {
		name        string
		wantRetired bool
		wantRun     bool
		wantError   bool
	}{
		{name: "live", wantRun: true},
		{name: "deleted while offline", wantRetired: true},
		{name: "replaced", wantRetired: true},
		{name: "deleting", wantRetired: true},
		{name: "unreconciled generation"},
		{name: "generation changes during validation"},
		{name: "disabled during validation"},
		{name: "namespace no longer watched"},
		{name: "API unavailable", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := uuid.New()
			ref := types.NamespacedName{Namespace: fmt.Sprintf("team-%d", i), Name: "daily"}
			projection := &database.ScheduledRun{
				ID: id.String(), Creator: "scheduler", Generation: 1, ResourceVersion: "1", Ref: ref, AgentUID: "agent-uid",
				Spec: v1alpha3.ScheduledRunSpec{AgentRef: corev1.LocalObjectReference{Name: "agent"}, Schedule: "* * * * *", Prompt: "hello"},
			}
			_, err := store.SyncScheduledRun(ctx, projection)
			require.NoError(t, err)
			accepted, err := store.TriggerScheduledRun(ctx, id, "operator", "accepted", 1)
			require.NoError(t, err)
			// Move only the occurrence clock; use store operations for all state transitions.
			_, err = db.Exec(ctx, `UPDATE scheduled_run SET next_execution_time = clock_timestamp() - interval '1 second' WHERE id = $1`, id)
			require.NoError(t, err)
			resource := &v1alpha3.ScheduledRun{ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name, UID: types.UID(id.String()), Generation: 1}, Spec: projection.Spec}
			objects := []client.Object{resource}
			namespaces := []string{ref.Namespace}
			switch test.name {
			case "deleted while offline":
				objects = nil
			case "replaced":
				resource.UID = types.UID(uuid.NewString())
			case "deleting":
				resource.Finalizers = []string{v1alpha3.ScheduledRunFinalizer}
				resource.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			case "unreconciled generation":
				resource.Generation++
			case "namespace no longer watched":
				namespaces = []string{"unrelated"}
			}
			reads := 0
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					switch test.name {
					case "API unavailable":
						return errors.New("API unavailable")
					case "generation changes during validation":
						projection.Generation++
						projection.Spec.Prompt = "new prompt"
						_, err := store.SyncScheduledRun(ctx, projection)
						require.NoError(t, err)
					case "disabled during validation":
						require.NoError(t, store.DisableScheduledRun(ctx, id, 1))
					}
					return reader.Get(ctx, key, obj, opts...)
				},
			}).Build()
			err = NewScheduler(store, kube, namespaces, time.Second).reserveDue(ctx)
			if test.wantError {
				require.ErrorContains(t, err, "API unavailable")
			} else {
				require.NoError(t, err)
			}
			if test.name == "namespace no longer watched" {
				require.Zero(t, reads, "excluded namespaces must be filtered before reading Kubernetes")
			}
			loaded, err := store.GetScheduledRun(ctx, id, "")
			require.NoError(t, err)
			require.Equal(t, test.wantRetired, loaded.DeletedAt != nil)
			if test.wantRetired {
				require.Nil(t, loaded.NextExecutionTime)
			}
			executions, err := store.ListScheduledRunExecutions(ctx, database.ScheduledRunExecutionQuery{ScheduledRunID: id, ScheduledRunQuery: database.ScheduledRunQuery{Limit: 10}})
			require.NoError(t, err)
			if test.wantRun {
				require.Len(t, executions, 2)
			} else {
				require.Len(t, executions, 1, "an unvalidated source must never accept a new cron occurrence")
			}
			replayed, err := store.TriggerScheduledRun(ctx, id, "operator", "accepted", 0)
			require.NoError(t, err)
			require.Equal(t, accepted.Id, replayed.Id, "accepted executions survive retirement and source changes")
		})
	}
}
