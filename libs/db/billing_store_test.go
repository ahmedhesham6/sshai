package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ahmedhesham6/sshai/libs/billing"
	dbstore "github.com/ahmedhesham6/sshai/libs/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreRegistersCreditRateHistoryIdempotently(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	effectiveAt := time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-small-v1", billing.ResourceCompute, "us-east-1", "small", "second", "2", effectiveAt,
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	registered, err := store.RegisterCreditRate(ctx, rate)
	if err != nil {
		t.Fatalf("register Credit Rate: %v", err)
	}
	replayed, err := store.RegisterCreditRate(ctx, rate)
	if err != nil {
		t.Fatalf("replay Credit Rate registration: %v", err)
	}
	if registered.Version() != rate.Version() || replayed.CreditsPerUnit() != "2" {
		t.Fatalf("registered Credit Rates = %#v, %#v", registered, replayed)
	}
	conflict, err := billing.NewCreditRate(
		rate.Version(), billing.ResourceCompute, "us-east-1", "small", "second", "3", effectiveAt,
	)
	if err != nil {
		t.Fatalf("create conflicting Credit Rate: %v", err)
	}
	if _, err := store.RegisterCreditRate(ctx, conflict); !errors.Is(err, dbstore.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Credit Rate registration error = %v", err)
	}
}

func TestStoreCreatesOneSharedCreditBalancePerUser(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-1", DefaultRegion: "us-east-1", ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil {
		t.Fatalf("load initial Credit Balance: %v", err)
	}
	if balance.UserID != "user-1" || balance.Credits != 0 || balance.Version != 0 || !balance.UpdatedAt.Equal(observedAt) {
		t.Fatalf("initial Credit Balance = %#v", balance)
	}
}

func TestStoreRegistersPolarWebhookBeforeProjectingSubscriptionAndCustomer(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-1", DefaultRegion: "us-east-1", ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	projection := billing.PolarSubscriptionProjection{
		ExternalEventID: "evt_subscription_01", EventType: billing.PolarSubscriptionCreated, OccurredAt: observedAt,
		SubscriptionID: "sub_01", CustomerID: "cus_01", ExternalCustomerID: "user-1", Status: "active",
		CurrentPeriodStart: observedAt, CurrentPeriodEnd: observedAt.Add(30 * 24 * time.Hour),
	}
	digest := sha256.Sum256([]byte(`{"type":"subscription.created","data":{"id":"sub_01"}}`))
	result, err := store.ApplyPolarProjection(ctx, projection, digest, observedAt)
	if err != nil {
		t.Fatalf("apply Polar subscription projection: %v", err)
	}
	if !result.Applied {
		t.Fatal("first Polar subscription projection was reported as a replay")
	}
	subscription, present, err := store.Subscription(ctx, "user-1")
	if err != nil || !present {
		t.Fatalf("load Subscription = %#v, %t, %v", subscription, present, err)
	}
	if subscription.PolarSubscriptionID != "sub_01" || subscription.Status != "active" || subscription.ExternalEventID != projection.ExternalEventID {
		t.Fatalf("Subscription projection = %#v", subscription)
	}
	customer, present, err := store.PolarCustomer(ctx, "user-1")
	if err != nil || !present || customer.PolarCustomerID != "cus_01" {
		t.Fatalf("Polar Customer projection = %#v, %t, %v", customer, present, err)
	}

	replay, err := store.ApplyPolarProjection(ctx, projection, digest, observedAt.Add(time.Minute))
	if err != nil || replay.Applied {
		t.Fatalf("replay Polar subscription projection = %#v, %v", replay, err)
	}
	conflictingDigest := sha256.Sum256([]byte(`{"type":"subscription.created","data":{"id":"sub_conflict"}}`))
	if _, err := store.ApplyPolarProjection(ctx, projection, conflictingDigest, observedAt.Add(2*time.Minute)); !errors.Is(err, dbstore.ErrPolarWebhookConflict) {
		t.Fatalf("conflicting Polar webhook error = %v", err)
	}
	assertPostgreSQLCode(t, execPoolError(ctx, pool, `DELETE FROM polar_webhook_receipts WHERE external_event_id = 'evt_subscription_01'`), "23514", "delete Polar webhook receipt")
}

func TestStoreProjectsRecurringPolarGrantIntoSharedCreditBalanceOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-1", DefaultRegion: "us-east-1", ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	projection := billing.PolarRecurringCreditGrantProjection{
		ExternalEventID: "evt_grant_01", OccurredAt: observedAt, GrantID: "benefit_grant_01",
		SubscriptionID: "sub_01", CustomerID: "cus_01", ExternalCustomerID: "user-1",
		MeterID: "meter_credits_used", Credits: 500, CreditedAt: observedAt,
	}
	digest := sha256.Sum256([]byte(`{"type":"benefit_grant.cycled","data":{"id":"benefit_grant_01"}}`))
	result, err := store.ApplyPolarProjection(ctx, projection, digest, observedAt)
	if err != nil {
		t.Fatalf("apply recurring Polar grant: %v", err)
	}
	if !result.Applied || result.CreditTransaction == nil || result.CreditTransaction.Credits() != 500 {
		t.Fatalf("recurring Polar grant result = %#v", result)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Credits != 500 || balance.Version != 1 {
		t.Fatalf("Credit Balance after recurring grant = %#v, %v", balance, err)
	}

	replay, err := store.ApplyPolarProjection(ctx, projection, digest, observedAt.Add(time.Minute))
	if err != nil || replay.Applied || replay.CreditTransaction != nil {
		t.Fatalf("recurring Polar grant replay = %#v, %v", replay, err)
	}
	balance, err = store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Credits != 500 || balance.Version != 1 {
		t.Fatalf("Credit Balance after recurring grant replay = %#v, %v", balance, err)
	}
}

func TestStoreSerializesConcurrentPolarWebhookReplay(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnsureUser(ctx, dbstore.EnsureUserInput{
		ID: "user-1", WorkOSUserID: "workos-1", DefaultRegion: "us-east-1", ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("ensure User: %v", err)
	}
	projection := billing.PolarRecurringCreditGrantProjection{
		ExternalEventID: "evt_grant_01", OccurredAt: observedAt, GrantID: "benefit_grant_01",
		SubscriptionID: "sub_01", CustomerID: "cus_01", ExternalCustomerID: "user-1",
		MeterID: "meter_credits_used", Credits: 500, CreditedAt: observedAt,
	}
	digest := sha256.Sum256([]byte(`{"type":"benefit_grant.cycled","data":{"id":"benefit_grant_01"}}`))
	start := make(chan struct{})
	results := make(chan struct {
		result dbstore.ApplyPolarProjectionResult
		err    error
	}, 2)
	for range 2 {
		go func() {
			<-start
			result, err := store.ApplyPolarProjection(ctx, projection, digest, observedAt)
			results <- struct {
				result dbstore.ApplyPolarProjectionResult
				err    error
			}{result, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Polar webhook errors = (%v, %v)", first.err, second.err)
	}
	if first.result.Applied == second.result.Applied {
		t.Fatalf("concurrent Polar webhook applied flags = (%t, %t), want exactly one", first.result.Applied, second.result.Applied)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Credits != 500 || balance.Version != 1 {
		t.Fatalf("Credit Balance after concurrent webhook = %#v, %v", balance, err)
	}
}

func TestStoreClosesComputeUsageIntoOneDebitBalanceAndPolarDelivery(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	startedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertBillingEnvironment(t, ctx, pool, "run_01")
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-standard-v1", billing.ResourceCompute, "us-east-1", "standard", "second", "2", startedAt.Add(-time.Hour),
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	if _, err := store.RegisterCreditRate(ctx, rate); err != nil {
		t.Fatalf("register Credit Rate: %v", err)
	}
	if _, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_01", UserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "run_01", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("open Compute Usage Interval: %v", err)
	}

	closedAt := startedAt.Add(time.Minute)
	transaction, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
		IntervalID: "cui_01", StoppedAt: closedAt, Source: dbstore.ComputeUsageClosedByRuntimeStop,
	})
	if err != nil {
		t.Fatalf("close Compute Usage Interval: %v", err)
	}
	if transaction.ID() != "credit-transaction:cui_01" || transaction.IdempotencyKey() != "compute:cui_01" || transaction.Credits() != -120 {
		t.Fatalf("Credit Transaction = id:%q key:%q credits:%d", transaction.ID(), transaction.IdempotencyKey(), transaction.Credits())
	}
	usage, debit := transaction.DebitUsage()
	if !debit || usage.EnvironmentID != "environment-1" || usage.ResourceID != "run_01" ||
		usage.RawQuantity != "60" || usage.RateVersion != rate.Version() {
		t.Fatalf("Credit Transaction usage = %#v, debit:%t", usage, debit)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil {
		t.Fatalf("load Credit Balance: %v", err)
	}
	if balance.Credits != -120 || balance.Version != 1 || !balance.UpdatedAt.Equal(closedAt) {
		t.Fatalf("Credit Balance = %#v", balance)
	}
	delivery, present, err := store.PolarDelivery(ctx, "compute:cui_01")
	if err != nil || !present {
		t.Fatalf("load PolarDelivery = %#v, %t, %v", delivery, present, err)
	}
	payload, err := json.Marshal(delivery.Event)
	if err != nil {
		t.Fatalf("marshal PolarDelivery event: %v", err)
	}
	if delivery.ExternalID != "compute:cui_01" || string(payload) != `{"name":"credits_used","external_customer_id":"user-1","timestamp":"2026-07-13T12:01:00Z","external_id":"compute:cui_01","metadata":{"credits":120,"resource_type":"compute","environment_id":"environment-1","region":"us-east-1","raw_quantity":"60","raw_unit":"second","rate_version":"compute-us-east-1-standard-v1"}}` {
		t.Fatalf("PolarDelivery = key:%q payload:%s", delivery.ExternalID, payload)
	}

	replayed, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
		IntervalID: "cui_01", StoppedAt: closedAt.Add(time.Minute), Source: dbstore.ComputeUsageClosedByProviderReconciliation,
	})
	if err != nil {
		t.Fatalf("replay Compute Usage close: %v", err)
	}
	if replayed.ID() != transaction.ID() || replayed.Credits() != transaction.Credits() || !replayed.OccurredAt().Equal(closedAt) {
		t.Fatalf("replayed Credit Transaction = %#v", replayed)
	}
	balance, err = store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Credits != -120 || balance.Version != 1 {
		t.Fatalf("Credit Balance after replay = %#v, %v", balance, err)
	}
}

func TestStoreSerializesConcurrentComputeUsageClosure(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	startedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertBillingEnvironment(t, ctx, pool, "run_01")
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-standard-v1", billing.ResourceCompute, "us-east-1", "standard", "second", "2", startedAt.Add(-time.Hour),
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	if _, err := store.RegisterCreditRate(ctx, rate); err != nil {
		t.Fatalf("register Credit Rate: %v", err)
	}
	if _, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_01", UserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "run_01", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("open Compute Usage Interval: %v", err)
	}

	start := make(chan struct{})
	results := make(chan struct {
		transaction billing.CreditTransaction
		err         error
	}, 2)
	for _, source := range []dbstore.ComputeUsageClosureSource{
		dbstore.ComputeUsageClosedByRuntimeStop,
		dbstore.ComputeUsageClosedByProviderReconciliation,
	} {
		go func() {
			<-start
			transaction, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
				IntervalID: "cui_01", StoppedAt: startedAt.Add(time.Minute), Source: source,
			})
			results <- struct {
				transaction billing.CreditTransaction
				err         error
			}{transaction, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent close errors = (%v, %v)", first.err, second.err)
	}
	if first.transaction.ID() != second.transaction.ID() || first.transaction.Credits() != -120 || second.transaction.Credits() != -120 {
		t.Fatalf("concurrent transactions = %#v, %#v", first.transaction, second.transaction)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Credits != -120 || balance.Version != 1 {
		t.Fatalf("Credit Balance after concurrent close = %#v, %v", balance, err)
	}
	if _, present, err := store.PolarDelivery(ctx, "compute:cui_01"); err != nil || !present {
		t.Fatalf("PolarDelivery after concurrent close = present:%t error:%v", present, err)
	}
}

func TestStoreReconcilesOrphanedProviderStoppedIntervalOnce(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	startedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertBillingEnvironment(t, ctx, pool, "run_01")
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-standard-v1", billing.ResourceCompute, "us-east-1", "standard", "second", "2", startedAt.Add(-time.Hour),
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	if _, err := store.RegisterCreditRate(ctx, rate); err != nil {
		t.Fatalf("register Credit Rate: %v", err)
	}
	if _, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_01", UserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "run_01", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("open Compute Usage Interval: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE environments SET current_runtime_id = NULL WHERE id = 'environment-1'`); err != nil {
		t.Fatalf("orphan Compute Usage Interval: %v", err)
	}

	first, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
		IntervalID: "cui_01", StoppedAt: startedAt.Add(time.Minute),
		Source: dbstore.ComputeUsageClosedByProviderReconciliation,
	})
	if err != nil {
		t.Fatalf("reconcile orphaned Compute Usage Interval: %v", err)
	}
	second, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
		IntervalID: "cui_01", StoppedAt: startedAt.Add(2 * time.Minute),
		Source: dbstore.ComputeUsageClosedByProviderReconciliation,
	})
	if err != nil || first.ID() != second.ID() || second.Credits() != -120 {
		t.Fatalf("replayed provider reconciliation = %#v, %v", second, err)
	}
	balance, err := store.CreditBalance(ctx, "user-1")
	if err != nil || balance.Version != 1 || balance.Credits != -120 {
		t.Fatalf("Credit Balance after reconciliation replay = %#v, %v", balance, err)
	}
}

func TestStoreRecordsPolarDeliverySuccessOnce(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	startedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertBillingEnvironment(t, ctx, pool, "run_01")
	rate, err := billing.NewCreditRate(
		"compute-us-east-1-standard-v1", billing.ResourceCompute, "us-east-1", "standard", "second", "2", startedAt.Add(-time.Hour),
	)
	if err != nil {
		t.Fatalf("create Credit Rate: %v", err)
	}
	if _, err := store.RegisterCreditRate(ctx, rate); err != nil {
		t.Fatalf("register Credit Rate: %v", err)
	}
	if _, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_01", UserID: "user-1", EnvironmentID: "environment-1", RuntimeID: "run_01", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("open Compute Usage Interval: %v", err)
	}
	closedAt := startedAt.Add(time.Minute)
	if _, err := store.CloseComputeUsageInterval(ctx, dbstore.CloseComputeUsageIntervalInput{
		IntervalID: "cui_01", StoppedAt: closedAt, Source: dbstore.ComputeUsageClosedByRuntimeStop,
	}); err != nil {
		t.Fatalf("close Compute Usage Interval: %v", err)
	}

	deliveredAt := closedAt.Add(time.Second)
	if err := store.RecordPolarDeliverySuccess(ctx, "compute:cui_01", deliveredAt); err != nil {
		t.Fatalf("record PolarDelivery success: %v", err)
	}
	if err := store.RecordPolarDeliverySuccess(ctx, "compute:cui_01", deliveredAt.Add(time.Minute)); err != nil {
		t.Fatalf("replay PolarDelivery success: %v", err)
	}
	completed, present, err := store.PolarDelivery(ctx, "compute:cui_01")
	if err != nil || !present {
		t.Fatalf("load completed PolarDelivery: %v", err)
	}
	replayed, present, err := store.PolarDelivery(ctx, "compute:cui_01")
	if err != nil || !present {
		t.Fatalf("reload completed PolarDelivery: %v", err)
	}
	if completed.DeliveredAt == nil || replayed.DeliveredAt == nil ||
		!completed.DeliveredAt.Equal(deliveredAt) || !replayed.DeliveredAt.Equal(deliveredAt) {
		t.Fatalf("PolarDelivery completion times = %v, %v", completed.DeliveredAt, replayed.DeliveredAt)
	}
	assertPostgreSQLCode(t, execPoolError(ctx, pool, `DELETE FROM polar_deliveries WHERE external_id = 'compute:cui_01'`), "23514", "delete PolarDelivery history")
}

func execPoolError(ctx context.Context, pool *pgxpool.Pool, statement string) error {
	_, err := pool.Exec(ctx, statement)
	return err
}

func TestStoreOpensComputeUsageOnlyForTheUsersCurrentRuntime(t *testing.T) {
	ctx := context.Background()
	store, pool := openTestStoreAndPool(t, ctx)
	startedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	insertBillingEnvironment(t, ctx, pool, "run_01")

	interval, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_01", UserID: "user-1", EnvironmentID: "environment-1",
		RuntimeID: "run_01", StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("open Compute Usage Interval: %v", err)
	}
	if interval.Region != "us-east-1" || interval.RuntimePreset != "standard" || interval.EndedAt != nil {
		t.Fatalf("open Compute Usage Interval = %#v", interval)
	}
	if _, err := store.OpenComputeUsageInterval(ctx, dbstore.OpenComputeUsageIntervalInput{
		ID: "cui_foreign", UserID: "user-1", EnvironmentID: "environment-1",
		RuntimeID: "run_replaced", StartedAt: startedAt,
	}); !errors.Is(err, dbstore.ErrRuntimeNotCurrent) {
		t.Fatalf("open replaced Runtime interval error = %v", err)
	}
}

func insertBillingEnvironment(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID string) {
	t.Helper()
	statements := []string{
		`INSERT INTO users (id, workos_user_id, default_region) VALUES ('user-1', 'workos-1', 'us-east-1')`,
		`INSERT INTO profiles (id, owner_user_id, name, slug) VALUES ('profile-1', 'user-1', 'Default', 'default')`,
		`INSERT INTO profile_versions (id, profile_id, version, digest) VALUES ('profile-version-1', 'profile-1', 1, 'sha256:' || repeat('c', 64))`,
		`INSERT INTO environments (
			id, owner_user_id, name, slug, lifecycle, health, region, availability_zone,
			runtime_preset, pinned_profile_version_id, version
		) VALUES ('environment-1', 'user-1', 'Workspace', 'workspace', 'active', 'healthy',
			'us-east-1', 'us-east-1a', 'standard', 'profile-version-1', 1)`,
		`INSERT INTO runtimes (
			id, environment_id, sequence, status, runtime_preset, region, availability_zone,
			image_version, provider_instance_ref, private_address, boot_id, started_at,
			created_at, updated_at, version
		) VALUES ('` + runtimeID + `', 'environment-1', 1, 'ready', 'standard', 'us-east-1',
			'us-east-1a', 'image-1', 'i-runtime-1', '10.0.0.4', 'boot-1', now(), now(), now(), 4)`,
		`UPDATE environments SET current_runtime_id = '` + runtimeID + `' WHERE id = 'environment-1'`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("insert billing prerequisite: %v", err)
		}
	}
}
