// tasks.go growth 域「任务」接口：列表查询 / 接受 / 领取奖励。
//
// 来源：上游 scripts/task_common.py 实测口径（list_tasks/accept_tasks/claim_reward），
// 集成进主程序后不再需要外部脚本。
//
// 端点（chatBase，BillingHeaders）：
//   - GET  /v2/activity/growth/tasks                全量任务列表（含 progress/accept_status）
//   - POST /v2/activity/growth/tasks/accept         {"task_codes":[...]} not_accepted → accepted
//   - POST /v2/activity/growth/tasks/reward/claim   {"task_code":"..."} 完成态领奖
//
// 语义要点：
//   - accept 是"报名"，不产生进度；进度由服务端行为事件点亮（如 chat_request_send 上报、
//     真实对话），故 accept 后可幂等重放。
//   - claim 仅在 progress 达标后可领；重复领返回业务错误（安全，不需要前置状态判断）。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth 域任务路径（与 scripts/task_common.py 对齐）。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
)

// Task 单个任务的对外视图（字段名与上游 JSON 对齐，多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"credit,omitempty"`      // 奖励积分（上游 reward_credit）
	Energy       int64  `json:"energy,omitempty"`      // 奖励能量（上游 reward_energy）
	HasReward    bool   `json:"has_reward,omitempty"`  // 是否带奖励
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"` // single（一次性）/ 累计型
	Tag          string `json:"tag,omitempty"`       // 端标记（PC 等）
	JumpURL      string `json:"jump_url,omitempty"`  // 客户端跳转协议（workbuddy://...）
	Locked       bool   `json:"locked,omitempty"`    // 上游标记未解锁
	Target       int64  `json:"target"`              // 目标次数（恒输出：0 是有效进度值）
	Current      int64  `json:"current"`             // 当前进度（恒输出：0 是有效进度值）
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`    // 上游任务状态（complete 等）
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取（accept_status == claimed）

	// 有效期字段（服务端实测下发，此前未解析）。上游对**已过期**任务仍会下发，
	// accept_status 停在 accepted、progress 停在 0/N——不区分的话它会永远显示为
	// 「待办」，把真正能做的任务挤掉。
	// 实测样例（wb_wechat_oa_subscribe_task）：valid_end=2026-11-30T23:59:00+08:00。
	ValidStart    string `json:"valid_start,omitempty"` // 任务开始时间（RFC3339），空=不限
	ValidEnd      string `json:"valid_end,omitempty"`   // 任务到期时间（RFC3339），空=永久
	IsNew         bool   `json:"is_new,omitempty"`      // 服务端标记的新任务（仅展示用）
	IsPinned      bool   `json:"is_pinned,omitempty"`   // 置顶（仅展示用）
	BadgeName     string `json:"badge_name,omitempty"`  // 徽标文案（仅展示用）
	TemplateIDFix int64  `json:"template_id_fixed,omitempty"` // 模板类任务的固定模板 id
}

// Expired 任务是否已过有效期（valid_end 早于 now）。
//
// 语义边界：
//   - valid_end 为空 = 永久任务，恒未过期；
//   - 解析失败按「未过期」处理——宁可多展示一项，也不要把一个其实能做的
//     任务静默藏掉（上游时间格式变过一次，不能让它变成功能的单点故障）。
func (t Task) Expired(now time.Time) bool {
	if t.ValidEnd == "" {
		return false
	}
	end, err := time.Parse(time.RFC3339, t.ValidEnd)
	if err != nil {
		return false
	}
	return now.After(end)
}

// ExpiresIn 距离到期还有多久（valid_end 为空或已过期返回 0）。供面板展示倒计时。
func (t Task) ExpiresIn(now time.Time) time.Duration {
	if t.ValidEnd == "" {
		return 0
	}
	end, err := time.Parse(time.RFC3339, t.ValidEnd)
	if err != nil {
		return 0
	}
	d := end.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// ListTasks 拉取全量任务列表。
// 响应形如 data.tasks[]，元素字段随任务类型变化（progress 可能是 {current,target} 或平铺），
// 这里做宽松解析：两种形状都尝试。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"` // 上游实际字段名（reward_ 前缀）
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
			ValidStart   string          `json:"valid_start"`
			ValidEnd     string          `json:"valid_end"`
			IsNew        bool            `json:"is_new"`
			IsPinned     bool            `json:"is_pinned"`
			BadgeName    string          `json:"badge_name"`
			TemplateID   int64           `json:"template_id_fixed"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress 可能是 {current,target} 对象（实测口径），覆盖平铺字段。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode:     t.TaskCode,
			Title:        t.Title,
			Description:  t.Description,
			TaskDesc:     t.TaskDesc,
			Credit:       t.RewardCredit,
			Energy:       t.RewardEnergy,
			HasReward:    t.HasReward,
			RewardBuddy:  t.RewardBuddy,
			TaskType:     t.TaskType,
			Tag:          t.Tag,
			JumpURL:      t.JumpURL,
			Locked:       t.Locked,
			Target:       tgt,
			Current:      cur,
			AcceptStatus: t.AcceptStatus,
			Status:       t.Status,
			Claimable:    !claimed && tgt > 0 && cur >= tgt,
			Claimed:      claimed,

			ValidStart:    t.ValidStart,
			ValidEnd:      t.ValidEnd,
			IsNew:         t.IsNew,
			IsPinned:      t.IsPinned,
			BadgeName:     t.BadgeName,
			TemplateIDFix: t.TemplateID,
		})
	}
	return out, nil
}

// AcceptTasks 接受任务（幂等：已 accepted 时上游返回成功或业务提示，均不视为致命错误）。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSON(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimReward 领取单个任务奖励。
//
// 端点来源（实测）：Web 成长中心的领奖请求 ——
//
//	POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim
//	（任务码在**路径**里，无 body；带 x-client-platform: web 头，Bearer 鉴权）
//
// 关键区别：此前误用 CLI 域 copilot.tencent.com 的
// /v2/activity/growth/tasks/reward/claim（task_code 放 body），该路径**不存在**，
// 一直返回 400 "task not completed"，是此前领奖失败的真实原因。
// 本实现返回 (credit, energy, err)：credit/energy 为本次到账奖励（已领取过时为 0）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.webBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	// Web 端请求头形状（对照浏览器实际请求）：Origin/Referer 指向 workbuddy.cn 成长中心，
	// 带 x-client-platform: web 标记来源端。
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	// 响应 data：{"already_claimed":bool,"credit":100,"energy":5,...}
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但无新增奖励
	}
	return resp.Credit, resp.Energy, nil
}
