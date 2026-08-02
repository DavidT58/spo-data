// Package dashweb serves the spo-dashboard UI and JSON API on localhost. nginx
// (basic auth) and Cloudflare sit in front; the daemon itself does no auth.
package dashweb

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"spo-data/configs"
	"spo-data/internal/leaderlog"
)

//go:embed web
var webFS embed.FS

// Server assembles API responses from the leaderlog store. It never talks to
// cardano-cli or Blockfrost itself — everything is read from SQLite plus the
// daemon's in-memory health snapshot.
type Server struct {
	store  *leaderlog.Store
	pools  func() []configs.PoolConfig
	set    leaderlog.Settings
	bfURL  string
	logger *log.Logger
}

func NewServer(store *leaderlog.Store, pools func() []configs.PoolConfig, set leaderlog.Settings, bfURL string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{store: store, pools: pools, set: set, bfURL: bfURL, logger: logger}
}

// Handler returns the http mux: embedded static UI at /, JSON under /api/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServerFS(static))
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/pool/", s.handlePool)
	mux.HandleFunc("/api/health", s.handleHealth)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// --- /api/overview ---

type overviewPool struct {
	PoolID         string     `json:"pool_id"`
	Name           string     `json:"name"`
	ScheduleStatus string     `json:"schedule_status"`
	Scheduled      int64      `json:"scheduled"`
	Produced       int64      `json:"produced"`
	Missed         int64      `json:"missed"`
	Pending        int64      `json:"pending"`
	NextSlotTime   *time.Time `json:"next_slot_time"`
	Anomalies      *int64     `json:"anomalies"` // nil => "n/a" (schedule not ok)
}

type overviewOperator struct {
	Name  string         `json:"name"`
	Pools []overviewPool `json:"pools"`
}

type overviewEpoch struct {
	Number      int       `json:"number"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	ProgressPct float64   `json:"progress_pct"`
}

type overviewResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Epoch       *overviewEpoch     `json:"epoch"`
	Operators   []overviewOperator `json:"operators"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	health := leaderlog.GetHealth()
	epoch := health.CurrentEpoch

	resp := overviewResponse{GeneratedAt: time.Now().UTC()}
	if info, found, err := s.store.GetEpochInfo(epoch); err == nil && found {
		pct := 0.0
		total := info.EndTime.Sub(info.StartTime).Seconds()
		if total > 0 {
			pct = time.Since(info.StartTime).Seconds() / total * 100
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
		}
		resp.Epoch = &overviewEpoch{Number: epoch, StartTime: info.StartTime, EndTime: info.EndTime, ProgressPct: pct}
	}

	byOperator := map[string]*overviewOperator{}
	var order []string
	for _, pool := range s.pools() {
		op, ok := byOperator[pool.Operator]
		if !ok {
			op = &overviewOperator{Name: pool.Operator}
			byOperator[pool.Operator] = op
			order = append(order, pool.Operator)
		}

		row := overviewPool{PoolID: pool.PoolID, Name: pool.Name, ScheduleStatus: leaderlog.SchedPending}
		if ps, found, err := s.store.GetPoolSchedule(pool.PoolID, epoch); err == nil && found {
			row.ScheduleStatus = ps.Status
		}
		if counts, err := s.store.CountSlots(pool.PoolID, epoch); err == nil {
			row.Scheduled = counts.Scheduled
			row.Produced = counts.Produced
			row.Missed = counts.Missed
			row.Pending = counts.Pending
		}
		if next, found, err := s.store.NextPendingSlotAfter(pool.PoolID, health.TipSlot); err == nil && found {
			t := next.SlotTime
			row.NextSlotTime = &t
		}
		if row.ScheduleStatus == leaderlog.SchedOK {
			if n, err := s.store.AnomalyCount(pool.PoolID, epoch); err == nil {
				row.Anomalies = &n
			}
		}
		op.Pools = append(op.Pools, row)
	}
	for _, name := range order {
		resp.Operators = append(resp.Operators, *byOperator[name])
	}
	writeJSON(w, resp)
}

// --- /api/pool/{id} ---

type slotRow struct {
	Slot         int64      `json:"slot"`
	Time         time.Time  `json:"time"`
	Status       string     `json:"status"`
	BlockHash    string     `json:"block_hash,omitempty"`
	BattleLeader string     `json:"battle_leader,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
}

type unscheduledBlock struct {
	Slot      int64     `json:"slot"`
	Time      time.Time `json:"time"`
	BlockHash string    `json:"block_hash"`
	Anomaly   bool      `json:"anomaly"` // false => not yet classifiable
}

type historyRow struct {
	Epoch       int      `json:"epoch"`
	Scheduled   int      `json:"scheduled"`
	Produced    int      `json:"produced"`
	Missed      int      `json:"missed"`
	Expected    float64  `json:"expected"`
	LuckPct     *float64 `json:"luck_pct"`
	ActualsOnly bool     `json:"actuals_only"`
	Finalized   bool     `json:"finalized"`
}

type poolResponse struct {
	PoolID         string             `json:"pool_id"`
	Name           string             `json:"name"`
	Operator       string             `json:"operator"`
	Epoch          int                `json:"epoch"`
	ScheduleStatus string             `json:"schedule_status"`
	ScheduleError  string             `json:"schedule_error,omitempty"`
	Slots          []slotRow          `json:"slots"`
	Unscheduled    []unscheduledBlock `json:"unscheduled_blocks"`
	History        []historyRow       `json:"history"`
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	poolID := strings.TrimPrefix(r.URL.Path, "/api/pool/")
	var pool *configs.PoolConfig
	for _, p := range s.pools() {
		if p.PoolID == poolID {
			pool = &p
			break
		}
	}
	if pool == nil {
		http.Error(w, "unknown pool", http.StatusNotFound)
		return
	}

	health := leaderlog.GetHealth()
	epoch := health.CurrentEpoch
	resp := poolResponse{
		PoolID: pool.PoolID, Name: pool.Name, Operator: pool.Operator,
		Epoch: epoch, ScheduleStatus: leaderlog.SchedPending,
		Slots: []slotRow{}, Unscheduled: []unscheduledBlock{}, History: []historyRow{},
	}
	if ps, found, err := s.store.GetPoolSchedule(pool.PoolID, epoch); err == nil && found {
		resp.ScheduleStatus = ps.Status
		resp.ScheduleError = ps.Error
	}

	if slots, err := s.store.SlotsForPoolEpoch(pool.PoolID, epoch); err == nil {
		for _, sl := range slots {
			resp.Slots = append(resp.Slots, slotRow{
				Slot: sl.Slot, Time: sl.SlotTime, Status: sl.Status,
				BlockHash: sl.BlockHash, BattleLeader: sl.BattleLeader, CheckedAt: sl.CheckedAt,
			})
		}
	}

	if obs, err := s.store.ObservedBlocks(pool.PoolID, epoch); err == nil {
		for _, b := range obs {
			if b.Scheduled == nil || !*b.Scheduled {
				resp.Unscheduled = append(resp.Unscheduled, unscheduledBlock{
					Slot: b.Slot, Time: b.Time, BlockHash: b.BlockHash,
					Anomaly: b.Scheduled != nil,
				})
			}
		}
	}

	if sums, err := s.store.SummariesForPool(pool.PoolID, 30); err == nil {
		for _, es := range sums {
			row := historyRow{
				Epoch: es.Epoch, Scheduled: es.Scheduled, Produced: es.Produced,
				Missed: es.Missed, Expected: es.ExpectedBlocks,
				ActualsOnly: es.ActualsOnly, Finalized: es.Finalized,
			}
			if es.ExpectedBlocks > 0 {
				base := float64(es.Scheduled)
				if es.ActualsOnly {
					base = float64(es.Produced)
				}
				luck := base / es.ExpectedBlocks * 100
				row.LuckPct = &luck
			}
			resp.History = append(resp.History, row)
		}
	}
	writeJSON(w, resp)
}

// --- /api/health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, struct {
		leaderlog.Health
		BlockfrostURL string `json:"blockfrost_url"`
	}{leaderlog.GetHealth(), s.bfURL})
}
