// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 开学季独立状态。解决"不知道哪些账号有哪些任务没做"与"开学季状态不可见"。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表 + 开学季任务列表，
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
//   - 开学季（school/status + school/run_all）：独立状态视图 + 一键闭环。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// schoolTaskView 开学季任务条目（面板展示口径）。
type schoolTaskView struct {
	Code   string `json:"task_code"`
	Status string `json:"status"` // pending | in_progress | completed | claimed
	Prog   int    `json:"progress"`
	Target int    `json:"target_count"`
}

// scanTaskView 扫描视图里的任务条目：upstream.Task + 一句「怎么做」的提示。
//
// 为什么需要 Hint：Manual 组的任务是「上游下发了但我们没有自动动作」，
// 光给任务码和标题用户不知道该点哪里。Hint 由 manualHint 生成（如公众号任务
// 写明「需关注满 24 小时」），前端直接展示在行尾。
type scanTaskView struct {
	upstream.Task
	Hint string `json:"hint,omitempty"`
}

// scanAccountItem 单账号扫描结果。
//
// Growth/Claimable/Manual/Expired 四组互斥，来源相同（服务端下发的任务列表），
// 由 classifyTask 分组。拆四组而不是一个列表，是因为它们的**下一步动作不同**：
//   - Growth    ：要跑行为动作（有 autoAction）；
//   - Claimable ：只需领奖（进度已达标）；
//   - Manual    ：上游下发了但我们没有对应动作 → 需要在官方客户端人工完成；
//   - Expired   ：已过 valid_end，做了也不计分 → 只提示，不进队列。
//
// 旧实现把后两组直接丢弃，导致「以为没有待办，其实是不会做/已过期」。
type scanAccountItem struct {
	UID       string           `json:"uid"`
	Nickname  string           `json:"nickname"`
	Growth    []scanTaskView   `json:"growth,omitempty"`
	Claimable []scanTaskView   `json:"claimable,omitempty"`
	Manual    []scanTaskView   `json:"manual,omitempty"`
	Expired   []scanTaskView   `json:"expired,omitempty"`
	GrowthErr string           `json:"growth_error,omitempty"`
	School    []schoolTaskView `json:"school,omitempty"`
	SchoolErr string           `json:"school_error,omitempty"`
	InPeriod  bool             `json:"in_period"`
}

// taskCapability 任务的分组结果（见 scanAccountItem 注释）。
type taskCapability int

const (
	capAuto    taskCapability = iota // 有自动动作，可完成
	capClaim                         // 已达标，只需领奖
	capManual                        // 无自动动作，需人工
	capExpired                       // 已过有效期
)

// classifyTask 按服务端下发的字段判定任务归属，**不依赖硬编码白名单**。
//
// 为什么不再用白名单当准入条件：上游会增删任务码（实测 9 个账号现已只剩 4 个码，
// 其中 1 个本仓库没有动作）。用「码在不在表里」决定要不要处理，结果是上游新增的
// 任务静默消失、上游下线的任务留下死代码——白名单必然过期。这里改成：
// 任务码只用来查动作（路由键），是否处理由**任务本身的状态**决定。
func classifyTask(t upstream.Task, now time.Time) taskCapability {
	if t.Claimed {
		return capClaim // 已领完：归类为「无需处理」，调用方自行跳过
	}
	if t.Expired(now) {
		return capExpired
	}
	if t.Target > 0 && t.Current >= t.Target {
		// 达标未领：必须入队等领奖。旧实现这里 return false，注释却写「也入队」——
		// 注释与代码矛盾，导致「进度满了却没领」的任务永远不出现在待办里。
		return capClaim
	}
	if t.Locked {
		return capManual // 上游锁定：动作会被拒，标手工而不是进队列空跑
	}
	if autoActionFor(t.TaskCode) != nil {
		return capAuto
	}
	return capManual
}

// growthPending 任务是否属于「可自动完成」组（保持旧签名，供既有调用点使用）。
func growthPending(t upstream.Task) bool {
	return classifyTask(t, time.Now()) == capAuto
}

// manualHint 给「需人工」任务生成一句可操作的提示（前端直接展示）。
func manualHint(t upstream.Task) string {
	switch t.TaskCode {
	case "wb_wechat_oa_subscribe_task":
		return "需用该账号绑定的微信关注「腾讯 WorkBuddy」公众号满 24 小时（纯人工，无接口）"
	case "task_student_verify":
		return "需微信学生真实认证，无法自动化"
	}
	if t.JumpURL != "" {
		return "无自动化动作，请在官方客户端按任务说明完成（" + t.JumpURL + "）"
	}
	return "无自动化动作，请在官方客户端按任务说明完成"
}

// schoolPending 开学季任务是否待办（排除学生认证）。
func schoolPending(t upstream.SchoolTask) bool {
	switch t.TaskCode {
	case "task_student_verify":
		return false // 需微信学生真实认证
	case "desktop_chat_1_time":
		return t.Status != "claimed"
	default:
		return t.Status != "claimed" && t.Status != "completed"
	}
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化）+ 开学季（未完成）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				// 统一用同一个 now 判定过期，避免同一批任务跨越到期时刻后组别不一致。
				now := time.Now()
				for _, t := range tasks {
					switch classifyTask(t, now) {
					case capAuto:
						it.Growth = append(it.Growth, scanTaskView{Task: t})
					case capClaim:
						if t.Claimed {
							continue // 已领完：不进任何待办组
						}
						it.Claimable = append(it.Claimable, scanTaskView{Task: t})
					case capExpired:
						it.Expired = append(it.Expired, scanTaskView{Task: t})
					default:
						// 需人工：附一句具体做法，前端直接展示。
						it.Manual = append(it.Manual, scanTaskView{Task: t, Hint: manualHint(t)})
					}
				}
			}
			if stasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a); err != nil {
				it.SchoolErr = err.Error()
			} else {
				it.InPeriod = inPeriod
				// 活动已结束（in_period=false）就不再把它的任务列为待办。
				// 上游对结束的活动仍会返回全部任务（status 停在 pending），
				// 不过滤的话它们会永远显示为「待执行」——实测 9 个账号共 36 项，
				// 把真正能做的任务和高优先级提示全挤到下面。
				if inPeriod {
					for _, t := range stasks {
						if schoolPending(t) {
							it.School = append(it.School, schoolTaskView{
								Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
							})
						}
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending, claimable, manual, expired := 0, 0, 0, 0
	for _, it := range items {
		pending += len(it.Growth) + len(it.School)
		claimable += len(it.Claimable)
		manual += len(it.Manual)
		expired += len(it.Expired)
	}
	log.Printf("panel: 队列扫描完成：可执行 %d 项、待领奖 %d 项、需人工 %d 项、已过期 %d 项",
		pending, claimable, manual, expired)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"accounts":      items,
		"pending_count": pending + claimable, // 兼容旧前端：可排队的总数
		"claim_count":   claimable,
		"manual_count":  manual,
		"expired_count": expired,
	})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth | school
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
		School      bool `json:"school"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth && !body.School {
		body.Growth, body.School = true, true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		writeErr(w, http.StatusConflict, "队列正在执行中（可在任务中心查看进度）")
		return
	}
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()
	type acct struct {
		a      *auth.Auth
		grow   []upstream.Task
		school bool
	}
	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, wantSchool bool) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if body.Growth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					now := time.Now()
					for _, t := range tasks {
						switch classifyTask(t, now) {
						case capAuto:
							one.grow = append(one.grow, t)
						case capClaim:
							if !t.Claimed {
								// 达标未领：只排队领奖，不重跑行为链——
								// 重跑会多消耗真实对话/上报配额，而进度早已满足。
								one.claim = append(one.claim, t)
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if wantSchool && p.cfg.Scheduler != nil {
				// in_period=false 时不入队：活动结束后 schoolAccount 会静默 return，
				// 队列却仍把它标记为 done「0/5 项已完成」——假成功比失败更误导。
				if stasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a); err == nil && inPeriod {
					for _, t := range stasks {
						if schoolPending(t) { // 认证等不可做任务已在口径外
							one.school = true
							break
						}
					}
				}
			}
			if len(one.grow) > 0 || len(one.claim) > 0 || one.school {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a, body.School)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
		for _, t := range one.claim {
			// claim_only：进度已达标，只需领奖。与 growth 分开是因为它**不跑行为链**——
			// 重跑一遍再领会多消耗真实对话/上报配额，而进度早已满足。
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "claim_only", Code: t.TaskCode, Status: "pending"})
		}
		if one.school {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "school", Code: "school_daily", Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": "全部账号没有可执行的待办任务（其余可能需人工或已过期）"})
		return
	}

	q.mu.Lock()
	q.running = true
	q.startedAt = time.Now()
	q.items = items
	q.conc = body.Concurrency
	q.seq++
	seq := q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, body.Concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v 开学季 %v）", len(items), body.Concurrency, body.Growth, body.School)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": len(items), "seq": seq})
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				case "claim_only":
					msg, err = p.claimOnlyQueued(one.a, code)
				case "school":
					msg, err = p.runSchoolQueued(one.a)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a      *auth.Auth
	grow   []upstream.Task // 需要跑行为动作的任务
	claim  []upstream.Task // 进度已达标、只需领奖的任务
	school bool
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	after, _ := p.taskByCodeWaiting(a, code)
	if after != nil && after.Claimable {
		if credit, energy, cerr := p.cfg.Upstream.ClaimReward(a, code); cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// claimOnlyQueued 只领奖：任务进度已达标，不重跑行为链。
//
// 为什么单独一条路径而不是复用 runGrowthQueued：后者会先跑 act.run()，
// 那一步可能含真实对话/专家召唤（消耗配额），而此时进度早就满了——
// 重跑既浪费上游配额，也可能因为任务已 claimed 而被服务端拒绝。
func (p *Panel) claimOnlyQueued(a *auth.Auth, code string) (string, error) {
	// 领奖前先确认当前状态：扫描到入队之间可能已被别的路径领掉（面板点击/其他队列）。
	t, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "该账号无此任务", nil
	}
	if t.Claimed {
		return "已完成（已领取）", nil
	}
	if !t.Claimable {
		// 进度在入队后回落（服务端偶发回滚）或未达标：如实说明，不当失败。
		return "入队后进度未达标（" + taskProgressText(t) + "），跳过领奖", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimReward(a, code)
	if err != nil {
		return "", fmt.Errorf("领奖失败: %w", err)
	}
	msg := "已领奖"
	if credit > 0 || energy > 0 {
		msg = fmt.Sprintf("已领奖 +%d 分 +%d 能", credit, energy)
	} else {
		msg = "奖励此前已领取（幂等）"
	}
	log.Printf("panel: 队列 claim_only uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// runSchoolQueued 单账号开学季闭环（scheduler 四任务 + 抽奖）。
func (p *Panel) runSchoolQueued(a *auth.Auth) (string, error) {
	if p.cfg.Scheduler == nil {
		return "", fmt.Errorf("scheduler 不可用")
	}
	p.cfg.Scheduler.RunSchoolAccountNow(a)
	// 闭环后回读开学季状态做汇总。
	tasks, _, err := p.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return "闭环已执行（状态回读失败）", nil
	}
	done := 0
	for _, t := range tasks {
		if t.Status == "claimed" || (t.TaskCode != "task_student_verify" && t.Progress >= t.TargetCount && t.TargetCount > 0) {
			done++
		}
	}
	log.Printf("panel: 队列 school uid=%s: 闭环完成（%d/%d 项完成）", a.UID, done, len(tasks))
	return fmt.Sprintf("开学季闭环完成（%d/%d 项已完成，抽奖已抽完）", done, len(tasks)), nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// ---------------------------------------------------------------------------
// 开学季独立视图
// ---------------------------------------------------------------------------

// schoolStatus 全账号开学季任务状态（含抽奖余额）。
func (p *Panel) schoolStatus(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	type acctView struct {
		UID      string           `json:"uid"`
		Nickname string           `json:"nickname"`
		InPeriod bool             `json:"in_period"`
		Tasks    []schoolTaskView `json:"tasks"`
		Chances  int              `json:"chances"`
		Err      string           `json:"error,omitempty"`
	}
	out := make([]acctView, 0, len(states))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			v := acctView{UID: a.UID, Nickname: a.Nickname}
			// D4 门控：global 账号无开学季活动，不发起任何上游调用。
			if a.IsGlobal() {
				v.Err = "global realm（无开学季活动）"
				mu.Lock()
				out = append(out, v)
				mu.Unlock()
				return
			}
			tasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a)
			if err != nil {
				v.Err = err.Error()
			} else {
				v.InPeriod = inPeriod
				for _, t := range tasks {
					v.Tasks = append(v.Tasks, schoolTaskView{
						Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
					})
				}
			}
			v.Chances, _ = p.cfg.Upstream.SchoolChances(a)
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Nickname < out[j].Nickname })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// schoolRunAll 一键执行全部账号开学季闭环（异步，进度看任务频道日志）。
func (p *Panel) schoolRunAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunSchoolNow()
	log.Printf("panel: 开学季全账号闭环已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}
