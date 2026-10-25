package database

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/scheduling"
)

func reserveTestDueScheduledRuns(t *testing.T, c *Client, limit int) error {
	t.Helper()
	sources, err := c.ListDueScheduledRuns(t.Context(), nil, limit)
	if err != nil {
		return err
	}
	return c.ReserveDueScheduledRuns(t.Context(), sources)
}

func createTestSchedule(t *testing.T, c *Client) *ScheduledRun {
	t.Helper()
	sessionFixture(t, c, t.Context(), "team-a", "scheduled-revision", "report", "runtime")
	request := &ScheduledRun{ID: uuid.NewString(), Creator: "alice", Generation: 1, ResourceVersion: "1", Ref: types.NamespacedName{Namespace: "team-a", Name: "daily"}, AgentUID: "report-uid", Spec: v1alpha3.ScheduledRunSpec{AgentRef: corev1.LocalObjectReference{Name: "report"}, Schedule: "* * * * *", Prompt: "original prompt"}}
	result, err := c.SyncScheduledRun(t.Context(), request)
	require.NoError(t, err)
	return result
}

func updateTestSchedule(t *testing.T, c *Client, schedule *ScheduledRun, config v1alpha3.ScheduledRunSpec) (*ScheduledRun, error) {
	t.Helper()
	changed := cloneTestSchedule(schedule)
	changed.Spec, changed.Generation, changed.ResourceVersion = config, schedule.Generation+1, uuid.NewString()
	return c.SyncScheduledRun(t.Context(), changed)
}

func listTestScheduleExecutions(t *testing.T, c *Client, schedule *ScheduledRun) []*apiv1.ScheduledRunExecution {
	t.Helper()
	executions, err := c.ListScheduledRunExecutions(t.Context(), ScheduledRunExecutionQuery{
		ScheduledRunQuery: ScheduledRunQuery{Creator: schedule.Creator, Limit: 100},
		ScheduledRunID:    uuid.MustParse(schedule.ID),
	})
	require.NoError(t, err)
	return executions
}

func TestScheduledExecutionLeasesFenceExpiredWorkers(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), schedule.Creator, "lease", schedule.Generation)
	require.NoError(t, err)
	leases := make(chan LeasedScheduledRunExecution, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			batch, err := c.LeaseScheduledRunExecutions(t.Context(), 1)
			if err != nil {
				t.Error(err)
				return
			}
			for _, lease := range batch {
				leases <- lease
			}
		})
	}
	wg.Wait()
	close(leases)
	require.Len(t, leases, 1, "only one controller can lease a firing")
	oldLease := <-leases
	require.Equal(t, execution.Id, oldLease.Execution.Id)
	// Move only the lease clock to simulate a controller dying. The firing was
	// created and all lifecycle transitions are exercised through store APIs.
	_, err = db.Exec(t.Context(), `UPDATE scheduled_run_execution SET next_attempt_at = clock_timestamp() - interval '1 second' WHERE id = $1`, execution.Id)
	require.NoError(t, err)
	oldLease.Execution.State = apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED
	require.ErrorIs(t, c.UpdateScheduledRunExecution(t.Context(), oldLease.Lease, ScheduledRunExecutionProgress{State: oldLease.Execution.State}), ErrConflict)
	batch, err := c.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	require.NotEqual(t, oldLease.Lease.Token, batch[0].Lease.Token)
	require.ErrorIs(t, c.UpdateScheduledRunExecution(t.Context(), oldLease.Lease, ScheduledRunExecutionProgress{State: oldLease.Execution.State}), ErrConflict)
	batch[0].Execution.State = apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED
	batch[0].Execution.FailureReason = "Controller denied execution"
	// A worker snapshot is not a write model: only explicit progress is saved.
	batch[0].Execution.Id = "replaced"
	batch[0].Execution.Prompt = "changed"
	batch[0].Execution.Deadline = nil
	batch[0].Execution.Creator = "mallory"
	batch[0].Execution.SessionId = "replaced"
	require.NoError(t, c.UpdateScheduledRunExecution(t.Context(), batch[0].Lease, ScheduledRunExecutionProgress{State: batch[0].Execution.State, FailureReason: batch[0].Execution.FailureReason}))
	finished, err := c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), schedule.Creator)
	require.NoError(t, err)
	require.NotNil(t, finished.CompletedAt)
	require.Equal(t, execution.Id, finished.Id)
	require.Equal(t, execution.Prompt, finished.Prompt)
	require.True(t, proto.Equal(execution.Deadline, finished.Deadline))
	require.Equal(t, execution.Creator, finished.Creator)
	require.Empty(t, finished.SessionId)
	require.Equal(t, batch[0].Execution.FailureReason, finished.FailureReason)
	batch, err = c.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Empty(t, batch, "completed work is never leased again")
}

func TestScheduledRunSnapshotsSurviveEditDeletionAndRecreation(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	schedule := createTestSchedule(t, c)
	id := uuid.MustParse(schedule.ID)
	accepted, err := c.TriggerScheduledRun(t.Context(), id, "operator", "manual", 1)
	require.NoError(t, err)
	require.Equal(t, "operator", accepted.Initiator)
	require.Equal(t, "report-uid", accepted.AgentUid)
	config := *schedule.Spec.DeepCopy()
	config.Prompt, config.Paused = "new prompt", new(true)
	updated, err := updateTestSchedule(t, c, schedule, config)
	require.NoError(t, err)
	require.Nil(t, updated.NextExecutionTime)
	stale, err := c.SyncScheduledRun(t.Context(), schedule)
	require.NoError(t, err)
	require.Equal(t, int64(2), stale.Generation)
	_, err = c.TriggerScheduledRun(t.Context(), id, "operator", "stale", 1)
	require.ErrorIs(t, err, ErrConflict)
	manual, err := c.TriggerScheduledRun(t.Context(), id, "operator", "paused", 2)
	require.NoError(t, err)
	require.Equal(t, "new prompt", manual.Prompt)
	require.NoError(t, c.RetireScheduledRun(t.Context(), id))
	require.NoError(t, c.RetireScheduledRun(t.Context(), id))
	replay, err := c.TriggerScheduledRun(t.Context(), id, "another-operator", "manual", 0)
	require.NoError(t, err)
	require.True(t, proto.Equal(accepted, replay))
	_, err = c.TriggerScheduledRun(t.Context(), id, "operator", "new", 0)
	require.ErrorIs(t, err, ErrFailedPrecondition)
	retired, err := c.SyncScheduledRun(t.Context(), updated)
	require.NoError(t, err)
	require.NotNil(t, retired.DeletedAt)
	replacement := cloneTestSchedule(schedule)
	replacement.ID = uuid.NewString()
	replacement.Spec.Prompt = "replacement prompt"
	_, err = c.SyncScheduledRun(t.Context(), replacement)
	require.NoError(t, err)
	history := listTestScheduleExecutions(t, c, schedule)
	require.Len(t, history, 2)
	require.Equal(t, "original prompt", history[1].Prompt)
	require.Equal(t, int64(1), history[1].ScheduledRunGeneration)
	require.Equal(t, &apiv1.ResourceReference{Namespace: schedule.Ref.Namespace, Name: schedule.Ref.Name}, history[1].ScheduledRun)
}

func TestScheduledRunSQLTimestamps(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	require.Equal(t, schedule.CreatedAt, schedule.UpdatedAt)
	next, err := scheduling.Next(schedule.Spec, schedule.CreatedAt)
	require.NoError(t, err)
	require.True(t, next.Equal(*schedule.NextExecutionTime))

	// Caller-supplied times cannot replace authoritative database timestamps.
	stale := cloneTestSchedule(schedule)
	stale.CreatedAt, stale.UpdatedAt = time.Unix(1, 0), time.Unix(2, 0)
	stale.NextExecutionTime = new(time.Unix(3, 0))
	loaded, err := c.SyncScheduledRun(t.Context(), stale)
	require.NoError(t, err)
	require.Equal(t, schedule, loaded)

	config := *schedule.Spec.DeepCopy()
	config.ExecutionTimeout = &metav1.Duration{Duration: 1500 * time.Nanosecond}
	config.Schedule = "*/5 * * * *"
	updated, err := updateTestSchedule(t, c, schedule, config)
	require.NoError(t, err)
	require.Equal(t, schedule.CreatedAt, updated.CreatedAt)
	require.True(t, updated.UpdatedAt.After(schedule.UpdatedAt))
	next, err = scheduling.Next(config, updated.UpdatedAt)
	require.NoError(t, err)
	require.True(t, next.Equal(*updated.NextExecutionTime))
	schedule = updated
	invalidConfig := *config.DeepCopy()
	invalidConfig.Schedule = "invalid cron"
	_, err = updateTestSchedule(t, c, schedule, invalidConfig)
	require.Error(t, err)
	loaded, err = c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice")
	require.NoError(t, err)
	require.Equal(t, updated, loaded, "failed cron calculation must roll back the update")
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "fractional-microsecond", schedule.Generation)
	require.NoError(t, err)
	require.Equal(t, 2*time.Microsecond, execution.Deadline.AsTime().Sub(execution.CreatedAt.AsTime()))
	require.NoError(t, c.RetireScheduledRun(t.Context(), uuid.MustParse(schedule.ID)))
	deleted, err := c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice")
	require.NoError(t, err)
	require.Equal(t, deleted.UpdatedAt, *deleted.DeletedAt)

	// A failed cron calculation must also roll back the preceding insert.
	invalid := cloneTestSchedule(schedule)
	invalid.Spec.Schedule = "invalid cron"
	invalid.ID = uuid.NewString()
	_, err = c.SyncScheduledRun(t.Context(), invalid)
	require.Error(t, err)
	_, err = c.GetScheduledRun(t.Context(), uuid.MustParse(invalid.ID), "alice")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestScheduledRunConcurrentReservation(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for range 12 {
		wg.Go(func() {
			run, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "same-request", schedule.Generation)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- run.Id
		})
	}
	wg.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	require.Len(t, unique, 1)

	_, err := db.Exec(t.Context(), "UPDATE scheduled_run SET next_execution_time = clock_timestamp() - interval '1 second' WHERE id = $1", schedule.ID)
	require.NoError(t, err)
	for range 12 {
		wg.Go(func() {
			if err := reserveTestDueScheduledRuns(t, c, 100); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	runs := listTestScheduleExecutions(t, c, schedule)
	require.Len(t, runs, 2, "one manual execution and one due occurrence")
	require.NotNil(t, runs[0].GetScheduledTime())
	require.False(t, unique[runs[0].Id])

	_, err = db.Exec(t.Context(), "UPDATE scheduled_run SET next_execution_time = clock_timestamp() - interval '2 minutes' WHERE id = $1", schedule.ID)
	require.NoError(t, err)
	require.NoError(t, reserveTestDueScheduledRuns(t, c, 100))
	require.Len(t, listTestScheduleExecutions(t, c, schedule), 2, "late occurrence must not create an execution")
	advanced, err := c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice")
	require.NoError(t, err)
	require.True(t, advanced.NextExecutionTime.After(time.Now()))
}

func TestScheduledRunConcurrentReconcileAndDelete(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	schedule := createTestSchedule(t, c)
	var wg sync.WaitGroup
	for generation := int64(2); generation < 10; generation++ {
		wg.Go(func() {
			changed := cloneTestSchedule(schedule)
			changed.Generation = generation
			_, err := c.SyncScheduledRun(t.Context(), changed)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() {
		if err := c.RetireScheduledRun(t.Context(), uuid.MustParse(schedule.ID)); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	retired, err := c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "")
	require.NoError(t, err)
	require.NotNil(t, retired.DeletedAt)
	require.Nil(t, retired.NextExecutionTime)
}

func TestScheduledRunExecutionConstraints(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	for _, update := range []string{
		"manual_request_id = NULL",
		"scheduled_time = clock_timestamp()", "state = 'UNKNOWN'", "state = 'SCHEDULED_RUN_EXECUTION_STATE_RUNNING'",
	} {
		_, err := db.Exec(t.Context(), "UPDATE scheduled_run_execution SET "+update+" WHERE id = $1", execution.Id)
		require.Error(t, err, update)
	}
	loaded, err := c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.True(t, proto.Equal(execution, loaded))
}

func TestScheduledExecutionSurvivesSessionDeletion(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	require.Empty(t, execution.SessionId)
	require.Equal(t, apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_PENDING, execution.State)
	// Concurrent worker retries must atomically reserve one session and its link.
	ids := make(chan string, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			linked, err := c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- linked.SessionId
		})
	}
	wg.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	require.Len(t, unique, 1)
	linked, err := c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.NotEmpty(t, linked.SessionId)
	require.NotEqual(t, execution.Id, linked.SessionId)
	session, err := c.GetSession(t.Context(), linked.SessionId, "alice")
	require.NoError(t, err)
	require.Equal(t, "scheduled-revision", session.PreparedRevision)
	require.NoError(t, deleteSession(t.Context(), c, session.Id))
	// Session deletion follows the ordinary hard-delete path.
	_, err = c.GetSession(t.Context(), session.Id, "alice")
	require.ErrorIs(t, err, ErrNotFound)
	replayed, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	require.True(t, proto.Equal(linked, replayed))
	retried, err := c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.True(t, proto.Equal(linked, retried))
	sessions, err := c.ListSessions(t.Context(), SessionQuery{UserID: "alice", Limit: 10})
	require.NoError(t, err)
	require.Empty(t, sessions)
	history, err := c.ListScheduledRunExecutions(t.Context(), ScheduledRunExecutionQuery{ScheduledRunQuery: ScheduledRunQuery{Creator: "alice", Limit: 10}, ScheduledRunID: uuid.MustParse(schedule.ID)})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.True(t, proto.Equal(linked, history[0]))
	_, err = c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "bob")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestScheduledExecutionWaitsForPreparedRevision(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	require.NoError(t, c.RetireAgentIdentities(t.Context(), "team-a", "report"))
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	_, err = c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.ErrorIs(t, err, ErrFailedPrecondition)
	loaded, err := c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.True(t, proto.Equal(execution, loaded))
	sessions, err := c.ListSessions(t.Context(), SessionQuery{UserID: "alice", Limit: 10})
	require.NoError(t, err)
	require.Empty(t, sessions)
	// Unready targets no longer roll back due reservations or block other schedules.
	_, err = db.Exec(t.Context(), "UPDATE scheduled_run SET next_execution_time = clock_timestamp() - interval '1 second' WHERE id = $1", schedule.ID)
	require.NoError(t, err)
	require.NoError(t, reserveTestDueScheduledRuns(t, c, 100))
	due := listTestScheduleExecutions(t, c, schedule)
	require.Len(t, due, 2, "manual and due executions survive unready targets")
	require.NotNil(t, due[0].GetScheduledTime())
	require.Empty(t, due[0].SessionId)
	sessionFixture(t, c, t.Context(), "team-a", "scheduled-revision-2", "report", "runtime")
	linked, err := c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	session, err := c.GetSession(t.Context(), linked.SessionId, "alice")
	require.NoError(t, err)
	require.Equal(t, "scheduled-revision-2", session.PreparedRevision)
}

func TestScheduledExecutionExpiresBeforeSessionCreation(t *testing.T) {
	c := NewClient(setupTestDB(t), "public")
	schedule := createTestSchedule(t, c)
	config := *schedule.Spec.DeepCopy()
	config.ExecutionTimeout = &metav1.Duration{Duration: time.Microsecond}
	updated, err := updateTestSchedule(t, c, schedule, config)
	schedule = updated
	require.NoError(t, err)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "expires", schedule.Generation)
	require.NoError(t, err)
	expired, err := c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.Empty(t, expired.SessionId)
	require.Equal(t, apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_TIMED_OUT, expired.State)
	replayed, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "expires", schedule.Generation)
	require.NoError(t, err)
	require.True(t, proto.Equal(expired, replayed))
}

func TestScheduledRunRejectsCorruptPayloads(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	for _, data := range [][]byte{{0xff}, {}} {
		_, err := db.Exec(t.Context(), "UPDATE scheduled_run_execution SET data = $1 WHERE id = $2", data, execution.Id)
		require.NoError(t, err)
		_, err = c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), "alice")
		require.Error(t, err)
		_, err = db.Exec(t.Context(), "UPDATE scheduled_run SET data = $1 WHERE id = $2", data, schedule.ID)
		require.NoError(t, err)
		_, err = c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice")
		require.Error(t, err)
	}
	// Valid JSON must retain the resolved defaults and required inputs. Reading
	// incomplete snapshots must fail instead of silently supplying new defaults.
	for _, tc := range []struct {
		name    string
		corrupt func(*scheduledRunSnapshot)
	}{
		{"prompt", func(s *scheduledRunSnapshot) { s.Spec.Prompt = " " }},
		{"timezone", func(s *scheduledRunSnapshot) { s.Spec.TimeZone = nil }},
		{"pause state", func(s *scheduledRunSnapshot) { s.Spec.Paused = nil }},
		{"timeout", func(s *scheduledRunSnapshot) { s.Spec.ExecutionTimeout = nil }},
		{"agent", func(s *scheduledRunSnapshot) { s.Spec.AgentRef.Name = "" }},
		{"agent UID", func(s *scheduledRunSnapshot) { s.AgentUID = "" }},
		{"resource version", func(s *scheduledRunSnapshot) { s.ResourceVersion = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := scheduledRunSnapshot{Spec: *schedule.Spec.DeepCopy(), ResourceVersion: schedule.ResourceVersion, AgentUID: schedule.AgentUID}
			tc.corrupt(&snapshot)
			data, err := json.Marshal(snapshot)
			require.NoError(t, err)
			_, err = db.Exec(t.Context(), "UPDATE scheduled_run SET data = $1 WHERE id = $2", data, schedule.ID)
			require.NoError(t, err)
			_, err = c.GetScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice")
			require.Error(t, err)
		})
	}
}

func TestScheduledExecutionTaskIdentityCannotChange(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	execution, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "manual", schedule.Generation)
	require.NoError(t, err)
	linked, err := c.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	leases, err := c.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	progress := ScheduledRunExecutionProgress{State: apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING, TaskID: execution.Id}
	require.NoError(t, c.UpdateScheduledRunExecution(t.Context(), leases[0].Lease, progress))
	require.ErrorIs(t, c.UpdateScheduledRunExecution(t.Context(), leases[0].Lease, progress), ErrConflict)
	_, err = db.Exec(t.Context(), "UPDATE scheduled_run_execution SET next_attempt_at = clock_timestamp() WHERE id = $1", execution.Id)
	require.NoError(t, err)
	leases, err = c.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	progress.TaskID = "another-task"
	require.ErrorIs(t, c.UpdateScheduledRunExecution(t.Context(), leases[0].Lease, progress), ErrConflict)
	// Omitting the already persisted task preserves its identity.
	progress.TaskID = ""
	progress.State = apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_SUCCEEDED
	require.NoError(t, c.UpdateScheduledRunExecution(t.Context(), leases[0].Lease, progress))
	loaded, err := c.GetScheduledRunExecution(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	require.Equal(t, execution.Id, loaded.TaskId)
	require.Equal(t, linked.SessionId, loaded.SessionId)
	require.Equal(t, progress.State, loaded.State)
}

func TestMalformedScheduledRunsDoNotBlockReservation(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	invalidCron := cloneTestSchedule(schedule)
	invalidCron.Spec.Schedule = "not a cron expression"
	cronData, err := json.Marshal(scheduledRunSnapshot{Spec: invalidCron.Spec, ResourceVersion: invalidCron.ResourceVersion, AgentUID: invalidCron.AgentUID})
	require.NoError(t, err)
	badData := [][]byte{{0xff}, {}, cronData}
	badIDs := []string{schedule.ID}
	for range 2 {
		badInput := cloneTestSchedule(schedule)
		badInput.ID = uuid.NewString()
		badInput.Ref.Name = badInput.ID
		bad, err := c.SyncScheduledRun(t.Context(), badInput)
		require.NoError(t, err)
		badIDs = append(badIDs, bad.ID)
	}
	goodInput := cloneTestSchedule(schedule)
	goodInput.ID = uuid.NewString()
	goodInput.Ref.Name = "healthy"
	good, err := c.SyncScheduledRun(t.Context(), goodInput)
	require.NoError(t, err)
	for i, id := range badIDs {
		_, err = db.Exec(t.Context(), `UPDATE scheduled_run SET data = $1 WHERE id = $2`, badData[i], id)
		require.NoError(t, err)
	}
	_, err = db.Exec(t.Context(), `UPDATE scheduled_run SET next_execution_time = created_at - interval '1 second'`)
	require.NoError(t, err)
	// A full batch of malformed rows must get out of the way of later rows.
	require.NoError(t, reserveTestDueScheduledRuns(t, c, 2))
	require.Empty(t, listTestScheduleExecutions(t, c, good))
	// A malformed config must not roll back a healthy row in the same batch.
	require.NoError(t, reserveTestDueScheduledRuns(t, c, 2))
	executions := listTestScheduleExecutions(t, c, good)
	require.Len(t, executions, 1)
	require.Equal(t, good.ID, executions[0].ScheduledRunId)
	for i, id := range badIDs {
		var data []byte
		var next *time.Time
		require.NoError(t, db.QueryRow(t.Context(), `SELECT data, next_execution_time FROM scheduled_run WHERE id = $1`, id).Scan(&data, &next))
		require.Equal(t, badData[i], data, "preserve malformed payloads for repair")
		require.Nil(t, next)
	}
	require.NoError(t, reserveTestDueScheduledRuns(t, c, 2))
	require.Equal(t, executions, listTestScheduleExecutions(t, c, good), "healthy occurrences must not be duplicated")
}

func TestMalformedScheduledExecutionsDoNotDiscardHealthyLeases(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	var badIDs []string
	for _, data := range [][]byte{{0xff}, {}} {
		bad, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), schedule.Creator, uuid.NewString(), schedule.Generation)
		require.NoError(t, err)
		_, err = db.Exec(t.Context(), `UPDATE scheduled_run_execution SET data = $1 WHERE id = $2`, data, bad.Id)
		require.NoError(t, err)
		badIDs = append(badIDs, bad.Id)
	}
	good, err := c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), schedule.Creator, "healthy", schedule.Generation)
	require.NoError(t, err)
	leases, err := c.LeaseScheduledRunExecutions(t.Context(), 8)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, good.Id, leases[0].Execution.Id)
	require.NoError(t, c.UpdateScheduledRunExecution(t.Context(), leases[0].Lease, ScheduledRunExecutionProgress{
		State: apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED, FailureReason: "test completion",
	}))
	leases, err = c.LeaseScheduledRunExecutions(t.Context(), 8)
	require.NoError(t, err)
	require.Empty(t, leases, "malformed rows retain their retry delay")
	// Repair a record and expire its lease; it must become eligible again.
	data, err := proto.Marshal(good)
	require.NoError(t, err)
	_, err = db.Exec(t.Context(), `UPDATE scheduled_run_execution SET data = $1, next_attempt_at = created_at WHERE id = $2`, data, badIDs[0])
	require.NoError(t, err)
	leases, err = c.LeaseScheduledRunExecutions(t.Context(), 8)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, badIDs[0], leases[0].Execution.Id)
}

func TestRetireMalformedScheduledRun(t *testing.T) {
	db := setupTestDB(t)
	c := NewClient(db, "public")
	schedule := createTestSchedule(t, c)
	_, err := db.Exec(t.Context(), `UPDATE scheduled_run SET data=$1 WHERE id=$2`, []byte{0xff}, schedule.ID)
	require.NoError(t, err)
	require.NoError(t, c.RetireScheduledRun(t.Context(), uuid.MustParse(schedule.ID)))
	require.NoError(t, c.RetireScheduledRun(t.Context(), uuid.MustParse(schedule.ID)))
	_, err = c.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "operator", "new", 1)
	require.ErrorIs(t, err, ErrFailedPrecondition)
}

func TestScheduledDispatchClaimSurvivesLeaseReplacement(t *testing.T) {
	db := setupTestDB(t)
	client := NewClient(db, "public")
	schedule := createTestSchedule(t, client)
	execution, err := client.TriggerScheduledRun(t.Context(), uuid.MustParse(schedule.ID), "alice", "dispatch", schedule.Generation)
	require.NoError(t, err)
	_, err = client.ReserveScheduledRunExecutionSession(t.Context(), uuid.MustParse(execution.Id), "alice")
	require.NoError(t, err)
	leases, err := client.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	lease := leases[0].Lease
	require.NoError(t, client.ClaimScheduledRunDispatch(t.Context(), lease))
	require.ErrorIs(t, client.ClaimScheduledRunDispatch(t.Context(), lease), ErrConflict)
	require.ErrorIs(t, client.UpdateScheduledRunExecution(t.Context(), lease, ScheduledRunExecutionProgress{State: apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_PENDING}), ErrConflict)
	require.NoError(t, client.UpdateScheduledRunExecution(t.Context(), lease, ScheduledRunExecutionProgress{State: apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING}))
	_, err = db.Exec(t.Context(), "UPDATE scheduled_run_execution SET next_attempt_at = clock_timestamp() WHERE id = $1", execution.Id)
	require.NoError(t, err)
	leases, err = client.LeaseScheduledRunExecutions(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.ErrorIs(t, client.ClaimScheduledRunDispatch(t.Context(), leases[0].Lease), ErrConflict)
	require.Equal(t, apiv1.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING, leases[0].Execution.State)
	require.Empty(t, leases[0].Execution.TaskId)
}

func cloneTestSchedule(schedule *ScheduledRun) *ScheduledRun {
	result := *schedule
	result.Spec = *schedule.Spec.DeepCopy()
	return &result
}

func TestDisabledScheduledRunRejectsNewExecutionsAndPreservesReplays(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			c := NewClient(setupTestDB(t), "public")
			schedule := createTestSchedule(t, c)
			schedule.Spec.Paused = new(paused)
			schedule, err := c.SyncScheduledRun(t.Context(), schedule)
			require.NoError(t, err)
			id := uuid.MustParse(schedule.ID)
			accepted, err := c.TriggerScheduledRun(t.Context(), id, "operator", "accepted", schedule.Generation)
			require.NoError(t, err)

			require.NoError(t, c.DisableScheduledRun(t.Context(), id, schedule.Generation+1))
			stale, err := c.SyncScheduledRun(t.Context(), schedule)
			require.NoError(t, err)
			require.True(t, stale.Disabled, "an older reconciliation cannot re-enable rejected config")
			require.Nil(t, stale.NextExecutionTime)
			_, err = c.TriggerScheduledRun(t.Context(), id, "operator", "rejected", stale.Generation)
			require.ErrorIs(t, err, ErrFailedPrecondition)
			replay, err := c.TriggerScheduledRun(t.Context(), id, "operator", "accepted", stale.Generation)
			require.NoError(t, err)
			require.True(t, proto.Equal(accepted, replay))

			schedule.Generation++
			schedule.Spec.Prompt = "repaired prompt"
			repaired, err := c.SyncScheduledRun(t.Context(), schedule)
			require.NoError(t, err)
			require.False(t, repaired.Disabled)
			fresh, err := c.TriggerScheduledRun(t.Context(), id, "operator", "repaired", repaired.Generation)
			require.NoError(t, err)
			require.Equal(t, "repaired prompt", fresh.Prompt)
			// A dependency can disappear and return without changing generation,
			// including on paused schedules whose due time stays nil throughout.
			require.NoError(t, c.DisableScheduledRun(t.Context(), id, repaired.Generation))
			_, err = c.TriggerScheduledRun(t.Context(), id, "operator", "dependency-missing", repaired.Generation)
			require.ErrorIs(t, err, ErrFailedPrecondition)
			resumed, err := c.SyncScheduledRun(t.Context(), repaired)
			require.NoError(t, err)
			require.False(t, resumed.Disabled)
			_, err = c.TriggerScheduledRun(t.Context(), id, "operator", "dependency-restored", resumed.Generation)
			require.NoError(t, err)
		})
	}
}
