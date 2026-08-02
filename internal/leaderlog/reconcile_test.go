package leaderlog

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spo-data/configs"
	"spo-data/internal/monitor"

	"github.com/blockfrost/blockfrost-go"
)

// --- stubs ---

type stubChain struct {
	pools      map[string]blockfrost.Pool
	history    map[string][]blockfrost.PoolHistory
	epochHash  map[string][]string          // key "epoch/pool" -> block hashes (desc)
	blocks     map[string]blockfrost.Block  // hash -> block
	slotBlocks map[int]blockfrost.Block     // slot -> block
	slotErr    map[int]error                // slot -> forced error
	latest     blockfrost.Block             // Blockfrost's own indexed tip
	poolErr    error
}

func (s *stubChain) BlockLatest(_ context.Context) (blockfrost.Block, error) {
	return s.latest, nil
}

func (s *stubChain) Pool(_ context.Context, id string) (blockfrost.Pool, error) {
	if s.poolErr != nil {
		return blockfrost.Pool{}, s.poolErr
	}
	return s.pools[id], nil
}
func (s *stubChain) PoolHistory(_ context.Context, id string, _ blockfrost.APIQueryParams) ([]blockfrost.PoolHistory, error) {
	return s.history[id], nil
}
func (s *stubChain) EpochBlockDistributionByPool(_ context.Context, epoch int, id string, q blockfrost.APIQueryParams) ([]string, error) {
	all := s.epochHash[fmt.Sprintf("%d/%s", epoch, id)]
	// crude paging
	start := (q.Page - 1) * q.Count
	if start >= len(all) {
		return nil, nil
	}
	end := start + q.Count
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}
func (s *stubChain) Block(_ context.Context, hash string) (blockfrost.Block, error) {
	b, ok := s.blocks[hash]
	if !ok {
		return blockfrost.Block{}, notFoundErr()
	}
	return b, nil
}
func (s *stubChain) BlockBySlot(_ context.Context, slot int) (blockfrost.Block, error) {
	if err, ok := s.slotErr[slot]; ok {
		return blockfrost.Block{}, err
	}
	b, ok := s.slotBlocks[slot]
	if !ok {
		return blockfrost.Block{}, notFoundErr()
	}
	return b, nil
}

func notFoundErr() error {
	return &blockfrost.APIError{Response: blockfrost.NotFound{StatusCode: 404}}
}

type stubCLI struct {
	tip      Tip
	tipErr   error
	schedule map[string][]ScheduleEntry // poolID -> entries
	schedErr map[string]error
	vrfHash  map[string]string // vkey path -> hash
}

func (s *stubCLI) Probe(context.Context) (string, error) { return "", nil }
func (s *stubCLI) Tip(context.Context) (Tip, error)      { return s.tip, s.tipErr }
func (s *stubCLI) LeadershipSchedule(_ context.Context, poolID, _ string) ([]ScheduleEntry, error) {
	if err, ok := s.schedErr[poolID]; ok {
		return nil, err
	}
	return s.schedule[poolID], nil
}
func (s *stubCLI) VRFKeyHash(_ context.Context, vkeyPath string) (string, error) {
	return s.vrfHash[vkeyPath], nil
}

// --- helpers ---

const (
	testEpoch  = 160
	epochLen   = 432000
	poolA      = "pool1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	poolAHash  = "hashA1"
)

func testSettings(t *testing.T, keysDir string) Settings {
	t.Helper()
	return Settings{
		KeysDir:           keysDir,
		SettleMarginSlots: 600,
		MaxAttempts:       3,
		BackfillEpochs:    10,
		CLITimeout:        time.Minute,
		Net: monitor.NetworkParams{
			SlotLength: 1, EpochLength: epochLen, ActiveSlotsCoeff: 0.05,
		},
	}
}

func newTestService(t *testing.T, chain *stubChain, cli *stubCLI) (*Service, *Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	svc := New(store, chain, cli, testSettings(t, keysDir), dir, "", log.New(os.Stderr, "", 0))
	svc.pools = []configs.PoolConfig{{Name: "TESTA", PoolID: poolA, Operator: "op"}}
	return svc, store, keysDir
}

func writeTestKeys(t *testing.T, keysDir, poolID string) {
	t.Helper()
	skey, vkey := KeyPaths(keysDir, poolID)
	for _, p := range []string{skey, vkey} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func epochSlot(offset int64) int64 { return int64(testEpoch)*epochLen + offset }

// testTip builds a consistent Tip for an epoch at the given intra-epoch offset
// (test slot numbering has no Byron prefix: startSlot = epoch × epochLen).
func testTip(epoch int, offset int64) Tip {
	return Tip{
		Epoch:           epoch,
		Slot:            int64(epoch)*epochLen + offset,
		SlotInEpoch:     offset,
		SlotsToEpochEnd: epochLen - offset,
		SyncProgress:    "100.00",
	}
}

// --- schedule pipeline tests ---

func TestComputePoolHappyPath(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}}
	cli := &stubCLI{
		tip: testTip(testEpoch, 5000),
		schedule: map[string][]ScheduleEntry{poolA: {
			{Slot: epochSlot(1000), SlotTime: time.Now().UTC()},
			{Slot: epochSlot(2000), SlotTime: time.Now().UTC()},
		}},
		vrfHash: map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "vrfhash"

	svc.schedulerTick(context.Background())

	ps, found, err := store.GetPoolSchedule(poolA, testEpoch)
	if err != nil || !found {
		t.Fatalf("schedule row missing: %v", err)
	}
	if ps.Status != SchedOK {
		t.Fatalf("status = %s (%s), want ok", ps.Status, ps.Error)
	}
	slots, _ := store.SlotsForPoolEpoch(poolA, testEpoch)
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
}

func TestComputePoolKeyMissing(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}}
	cli := &stubCLI{tip: testTip(testEpoch, 5000)}
	svc, store, _ := newTestService(t, chain, cli)

	svc.schedulerTick(context.Background())

	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedKeyMissing {
		t.Fatalf("status = %s, want key_missing", ps.Status)
	}
}

func TestComputePoolKeyStale(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "chainhash"}}}
	cli := &stubCLI{
		tip:     testTip(testEpoch, 5000),
		vrfHash: map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "localhash"

	svc.schedulerTick(context.Background())

	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedKeyStale {
		t.Fatalf("status = %s, want key_stale", ps.Status)
	}
}

func TestComputePoolBlockfrostDownIsRetryable(t *testing.T) {
	chain := &stubChain{poolErr: fmt.Errorf("connection refused")}
	cli := &stubCLI{
		tip: testTip(testEpoch, 5000),
		schedule: map[string][]ScheduleEntry{poolA: {
			{Slot: epochSlot(1000), SlotTime: time.Now().UTC()},
		}},
		vrfHash: map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "vrfhash"

	// A sustained Blockfrost outage across many scheduler passes must never
	// exhaust the attempt budget (attempts are refunded on Blockfrost errors).
	for i := 0; i < 5; i++ {
		svc.schedulerTick(context.Background())
		ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
		if ps.Status != SchedPending {
			t.Fatalf("pass %d: status = %s, want pending (retryable)", i, ps.Status)
		}
		if ps.Attempts != 0 {
			t.Fatalf("pass %d: attempts = %d, want 0 (refunded)", i, ps.Attempts)
		}
		// Age StartedAt past the cli timeout so the next pass retries.
		old := time.Now().Add(-2 * s_cliTimeout(svc))
		ps.StartedAt = &old
		if err := store.SavePoolSchedule(&ps); err != nil {
			t.Fatal(err)
		}
	}

	// Blockfrost recovers: the schedule must still land.
	chain.poolErr = nil
	chain.pools = map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}
	svc.schedulerTick(context.Background())
	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedOK {
		t.Fatalf("status after recovery = %s (%s), want ok", ps.Status, ps.Error)
	}
}

func s_cliTimeout(svc *Service) time.Duration { return svc.set.CLITimeout }

func TestExpiredScheduleFinalizesActualsOnly(t *testing.T) {
	svc, store, chain, cli := reconcilerFixture(t)
	// Epoch 160's schedule never landed (pending, attempts exhausted); the pool
	// still produced one real block, already ingested.
	if err := store.SavePoolSchedule(&PoolSchedule{
		PoolID: poolA, Epoch: testEpoch, Name: "TESTA", Status: SchedPending, Attempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveObservedBlock(&ObservedBlock{
		BlockHash: poolAHash, PoolID: poolA, Epoch: testEpoch, Slot: epochSlot(1500), Height: 5,
	}); err != nil {
		t.Fatal(err)
	}

	// Roll into the next epoch and finalize.
	tip := testTip(testEpoch+1, 5000)
	cli.tip = tip
	chain.latest = blockfrost.Block{Slot: int(tip.Slot)}
	svc.finalizeSummaries(context.Background(), tip)

	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedExpired {
		t.Fatalf("status = %s, want expired", ps.Status)
	}
	es, found, _ := store.GetEpochSummary(poolA, testEpoch)
	if !found || !es.Finalized || !es.ActualsOnly {
		t.Fatalf("summary = %+v, want finalized actuals-only", es)
	}
	if es.Produced != 1 || es.Scheduled != 0 {
		t.Fatalf("summary counts = sched %d / prod %d, want 0/1 (real production preserved)", es.Scheduled, es.Produced)
	}

	// The zombie must be gone: the epoch no longer demands reconcile work.
	epochs, _ := store.EpochsNeedingReconcile(tip.Epoch)
	for _, e := range epochs {
		if e == testEpoch {
			t.Fatalf("epoch %d still in EpochsNeedingReconcile after finalization", testEpoch)
		}
	}
}

func TestComputePoolSlotOutsideEpochWindow(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}}
	cli := &stubCLI{
		tip: testTip(testEpoch, 5000),
		schedule: map[string][]ScheduleEntry{poolA: {
			{Slot: epochSlot(-10), SlotTime: time.Now()}, // previous epoch — lagging node
		}},
		vrfHash: map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "vrfhash"

	svc.schedulerTick(context.Background())

	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedCLIError {
		t.Fatalf("status = %s (%s), want cli_error", ps.Status, ps.Error)
	}
}

func TestStuckPendingRecovery(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}}
	cli := &stubCLI{
		tip: testTip(testEpoch, 5000),
		schedule: map[string][]ScheduleEntry{poolA: {
			{Slot: epochSlot(1000), SlotTime: time.Now().UTC()},
		}},
		vrfHash: map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "vrfhash"

	// Simulate a crash mid-run: pending row, StartedAt older than CLITimeout.
	old := time.Now().Add(-2 * time.Hour)
	if err := store.SavePoolSchedule(&PoolSchedule{
		PoolID: poolA, Epoch: testEpoch, Status: SchedPending, Attempts: 1, StartedAt: &old,
	}); err != nil {
		t.Fatal(err)
	}

	svc.schedulerTick(context.Background())

	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedOK {
		t.Fatalf("status = %s (%s), want ok after recovery", ps.Status, ps.Error)
	}
	if ps.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", ps.Attempts)
	}
}

func TestCLIErrorRetriesBounded(t *testing.T) {
	chain := &stubChain{pools: map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}}}
	cli := &stubCLI{
		tip:      testTip(testEpoch, 5000),
		schedErr: map[string]error{poolA: fmt.Errorf("boom")},
		vrfHash:  map[string]string{},
	}
	svc, store, keysDir := newTestService(t, chain, cli)
	writeTestKeys(t, keysDir, poolA)
	_, vkey := KeyPaths(keysDir, poolA)
	cli.vrfHash[vkey] = "vrfhash"

	for i := 0; i < 5; i++ {
		svc.schedulerTick(context.Background())
	}
	ps, _, _ := store.GetPoolSchedule(poolA, testEpoch)
	if ps.Status != SchedCLIError {
		t.Fatalf("status = %s, want cli_error", ps.Status)
	}
	if ps.Attempts != 3 {
		t.Fatalf("attempts = %d, want capped at 3", ps.Attempts)
	}
}

// --- reconciler tests ---

func reconcilerFixture(t *testing.T) (*Service, *Store, *stubChain, *stubCLI) {
	chain := &stubChain{
		pools:      map[string]blockfrost.Pool{poolA: {VrfKey: "vrfhash"}},
		epochHash:  map[string][]string{},
		blocks:     map[string]blockfrost.Block{},
		slotBlocks: map[int]blockfrost.Block{},
		slotErr:    map[int]error{},
		history:    map[string][]blockfrost.PoolHistory{},
	}
	cli := &stubCLI{tip: testTip(testEpoch, 10000)}
	chain.latest = blockfrost.Block{Slot: int(cli.tip.Slot)}
	svc, store, _ := newTestService(t, chain, cli)
	return svc, store, chain, cli
}

// seedSchedule inserts an ok schedule with the given slots.
func seedSchedule(t *testing.T, store *Store, slots ...int64) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.SavePoolSchedule(&PoolSchedule{
		PoolID: poolA, Epoch: testEpoch, Status: SchedOK, ComputedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	rows := make([]ScheduledSlot, 0, len(slots))
	for _, sl := range slots {
		rows = append(rows, ScheduledSlot{
			PoolID: poolA, Epoch: testEpoch, Slot: sl,
			SlotTime: time.Now().UTC(), Status: SlotPending,
		})
	}
	if err := store.ReplaceScheduledSlots(poolA, testEpoch, rows); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProducedAndMissed(t *testing.T) {
	svc, store, chain, _ := reconcilerFixture(t)
	// Three due slots: one produced by us, one taken by another pool, one empty.
	produced, battle, empty := epochSlot(1000), epochSlot(2000), epochSlot(3000)
	seedSchedule(t, store, produced, battle, empty)

	chain.epochHash[fmt.Sprintf("%d/%s", testEpoch, poolA)] = []string{poolAHash}
	chain.blocks[poolAHash] = blockfrost.Block{
		Hash: poolAHash, Slot: int(produced), Height: 500, Time: int(time.Now().Unix()), SlotLeader: poolA,
	}
	chain.slotBlocks[int(produced)] = chain.blocks[poolAHash]
	chain.slotBlocks[int(battle)] = blockfrost.Block{Hash: "other", Slot: int(battle), SlotLeader: "pool1other"}
	// `empty` has no block: BlockBySlot 404s.

	svc.reconcileTick(context.Background())

	check := func(slot int64, wantStatus, wantBattle string) {
		t.Helper()
		sl, found, err := store.FindScheduledSlot(poolA, testEpoch, slot)
		if err != nil || !found {
			t.Fatalf("slot %d missing: %v", slot, err)
		}
		if sl.Status != wantStatus {
			t.Errorf("slot %d status = %s, want %s", slot, sl.Status, wantStatus)
		}
		if sl.BattleLeader != wantBattle {
			t.Errorf("slot %d battleLeader = %q, want %q", slot, sl.BattleLeader, wantBattle)
		}
	}
	check(produced, SlotProduced, "")
	check(battle, SlotMissed, "pool1other")
	check(empty, SlotMissed, "")

	// The produced block is recorded and classified as scheduled.
	obs, err := store.ObservedBlocks(poolA, testEpoch)
	if err != nil || len(obs) != 1 {
		t.Fatalf("observed blocks: %d (%v)", len(obs), err)
	}
	if obs[0].Scheduled == nil || !*obs[0].Scheduled {
		t.Errorf("observed block not classified as scheduled")
	}
}

func TestReconcileTransientErrorLeavesPending(t *testing.T) {
	svc, store, chain, _ := reconcilerFixture(t)
	due := epochSlot(1000)
	seedSchedule(t, store, due)
	chain.slotErr[int(due)] = fmt.Errorf("503 backend down")

	svc.reconcileTick(context.Background())

	sl, _, _ := store.FindScheduledSlot(poolA, testEpoch, due)
	if sl.Status != SlotPending {
		t.Fatalf("slot status = %s, want pending after transient error", sl.Status)
	}
}

func TestReconcileNotDueStaysPending(t *testing.T) {
	svc, store, _, cli := reconcilerFixture(t)
	// Slot within the settle margin of the tip: not yet due.
	notDue := cli.tip.Slot - 100
	seedSchedule(t, store, notDue)

	svc.reconcileTick(context.Background())

	sl, _, _ := store.FindScheduledSlot(poolA, testEpoch, notDue)
	if sl.Status != SlotPending {
		t.Fatalf("slot status = %s, want pending (inside settle margin)", sl.Status)
	}
}

func TestReconcilePreviousEpochTailSettles(t *testing.T) {
	svc, store, chain, cli := reconcilerFixture(t)
	// Tip rolled into the next epoch; a tail slot of the previous epoch is due.
	cli.tip = testTip(testEpoch+1, 5000)
	chain.latest = blockfrost.Block{Slot: int(cli.tip.Slot)}
	tail := epochSlot(epochLen - 100) // last 100 slots of testEpoch
	seedSchedule(t, store, tail)

	svc.reconcileTick(context.Background())

	sl, _, _ := store.FindScheduledSlot(poolA, testEpoch, tail)
	if sl.Status != SlotMissed {
		t.Fatalf("previous-epoch tail slot = %s, want missed (settled)", sl.Status)
	}
}

func TestAnomalyGatingWithoutOKSchedule(t *testing.T) {
	svc, store, chain, _ := reconcilerFixture(t)
	// No schedule at all (computation hasn't landed): pool produces a block.
	slot := epochSlot(1500)
	chain.epochHash[fmt.Sprintf("%d/%s", testEpoch, poolA)] = []string{poolAHash}
	chain.blocks[poolAHash] = blockfrost.Block{
		Hash: poolAHash, Slot: int(slot), Height: 500, Time: int(time.Now().Unix()), SlotLeader: poolA,
	}

	svc.reconcileTick(context.Background())

	obs, _ := store.ObservedBlocks(poolA, testEpoch)
	if len(obs) != 1 {
		t.Fatalf("want 1 observed block, got %d", len(obs))
	}
	if obs[0].Scheduled != nil {
		t.Fatalf("block classified (%v) before schedule ok — must stay NULL", *obs[0].Scheduled)
	}

	// Now the schedule lands WITHOUT that slot: rematch must flag the anomaly.
	seedSchedule(t, store, epochSlot(9000))
	svc.rematchObserved(poolA, testEpoch)

	obs, _ = store.ObservedBlocks(poolA, testEpoch)
	if obs[0].Scheduled == nil || *obs[0].Scheduled {
		t.Fatalf("want anomaly (scheduled=false) after rematch, got %v", obs[0].Scheduled)
	}
	n, _ := store.AnomalyCount(poolA, testEpoch)
	if n != 1 {
		t.Fatalf("anomaly count = %d, want 1", n)
	}
}

func TestRematchSettlesProducedSlot(t *testing.T) {
	svc, store, chain, _ := reconcilerFixture(t)
	slot := epochSlot(1500)
	chain.epochHash[fmt.Sprintf("%d/%s", testEpoch, poolA)] = []string{poolAHash}
	chain.blocks[poolAHash] = blockfrost.Block{
		Hash: poolAHash, Slot: int(slot), Height: 500, Time: int(time.Now().Unix()), SlotLeader: poolA,
	}
	// Block observed pre-schedule…
	svc.reconcileTick(context.Background())
	// …then the schedule lands WITH that slot.
	seedSchedule(t, store, slot)
	svc.rematchObserved(poolA, testEpoch)

	sl, _, _ := store.FindScheduledSlot(poolA, testEpoch, slot)
	if sl.Status != SlotProduced || sl.BlockHash != poolAHash {
		t.Fatalf("slot = %s/%s, want produced/%s", sl.Status, sl.BlockHash, poolAHash)
	}
}

func TestBackfillActualsOnly(t *testing.T) {
	svc, store, chain, cli := reconcilerFixture(t)
	chain.history[poolA] = []blockfrost.PoolHistory{
		{Epoch: testEpoch, Blocks: 5, ActiveSize: 0.01},     // current — skipped
		{Epoch: testEpoch - 1, Blocks: 20, ActiveSize: 0.01},
		{Epoch: testEpoch - 2, Blocks: 25, ActiveSize: 0.012},
	}
	// Backfill runs in the scheduler pass (before any summary can be created).
	cli.schedErr = map[string]error{poolA: fmt.Errorf("not under test")}
	cli.vrfHash = map[string]string{}
	svc.schedulerTick(context.Background())

	sums, err := store.SummariesForPool(poolA, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 2 backfilled past epochs + the current epoch's actuals-only fallback row
	// (schedule not ok), newest first.
	if len(sums) != 3 {
		t.Fatalf("want 3 summaries, got %d: %+v", len(sums), sums)
	}
	if sums[0].Epoch != testEpoch || !sums[0].ActualsOnly || sums[0].Finalized {
		t.Errorf("current-epoch fallback row wrong: %+v", sums[0])
	}
	for _, es := range sums[1:] {
		if !es.ActualsOnly || !es.Finalized {
			t.Errorf("epoch %d: actualsOnly=%v finalized=%v, want true/true", es.Epoch, es.ActualsOnly, es.Finalized)
		}
	}
	if sums[1].Produced != 20 || sums[1].ExpectedBlocks == 0 {
		t.Errorf("newest backfill row: produced=%d expected=%.1f", sums[1].Produced, sums[1].ExpectedBlocks)
	}
}

func TestFinalizePreviousEpochSummary(t *testing.T) {
	svc, store, chain, cli := reconcilerFixture(t)
	chain.history[poolA] = []blockfrost.PoolHistory{{Epoch: testEpoch, Blocks: 2, ActiveSize: 0.01}}
	produced, missed := epochSlot(1000), epochSlot(2000)
	seedSchedule(t, store, produced, missed)
	chain.slotBlocks[int(produced)] = blockfrost.Block{Hash: poolAHash, Slot: int(produced), SlotLeader: poolA}

	// Roll into the next epoch, settle, then finalize via the scheduler tick.
	cli.tip = testTip(testEpoch+1, 5000)
	cli.schedule = map[string][]ScheduleEntry{} // no slots next epoch
	cli.vrfHash = map[string]string{}
	svc.reconcileTick(context.Background())

	svc.finalizeSummaries(context.Background(), cli.tip)

	es, found, err := store.GetEpochSummary(poolA, testEpoch)
	if err != nil || !found {
		t.Fatalf("summary missing: %v", err)
	}
	if !es.Finalized {
		t.Fatal("summary not finalized after all slots settled")
	}
	if es.Scheduled != 2 || es.Produced != 1 || es.Missed != 1 {
		t.Fatalf("summary counts = %d/%d/%d, want 2/1/1", es.Scheduled, es.Produced, es.Missed)
	}
}
