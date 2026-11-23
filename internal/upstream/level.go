// level.go 等级任务（「养虾」系列）：与成长任务**并行**的另一套任务清单。
//
// 来源：GET {chatBase}/activity/growth/tasks —— 注意**没有 /v2 前缀**，
// 与成长任务的 /v2/activity/growth/tasks 是两个不同端点，且不互为超集
// （实测：/v2 返回 4 个成长任务，本端点返回 5 个等级任务，task_code 无一重叠）。
//
// 与成长任务的结构差异（2026-11 实测，9 个 cn 账号 + 1 个 global）：
//   - 字段名不同：本端点用 code/status，成长任务用 task_code/accept_status；
//   - 没有 progress / reward_credit / reward_energy / valid_end；
//   - status 实测**恒为 "available"**，不代表"未完成"，不能拿来判断进度；
//   - 完成度只能从 /activity/growth/profile 的 level/completed 读。
//
// 因此本模块只做「发现 + 展示」：列表与等级进度都是只读。
// 不做自动完成、不领奖——没有 progress 字段就没有判据，也没有找到对应 claim 接口。
// 参考实现（L0NE-6/WorkBuddy-Daily、XDTrees/dsh-xdpool）同样只把这类任务列为
// 「只发现」，不尝试自动化。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// levelTasksPath 等级任务列表（无 /v2 前缀；cn 走 copilot.tencent.com，
// global 走 www.workbuddy.ai，由 chatBase 决定）。
const levelTasksPath = "/activity/growth/tasks"

// levelProfilePath 等级进度（completed/level/level_name）。
const levelProfilePath = "/activity/growth/profile"

// LevelTask 等级任务条目（字段与上游 JSON 对齐，多余字段不透出）。
type LevelTask struct {
	TaskID      int64  `json:"task_id"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	IconURL     string `json:"icon_url,omitempty"`
	LevelName   string `json:"level_name,omitempty"` // 该任务对应的等级名（养虾尝试…控虾大神）
	Status      string `json:"status,omitempty"`     // 实测恒为 available，不代表未完成
	URL         string `json:"url,omitempty"`
}

// LevelProfile 等级进度。completed/total 是**等级任务**口径，
// 与成长任务列表里的任务数无关（同一账号两套 profile 会给出不同数字）。
type LevelProfile struct {
	Completed  int    `json:"completed"`
	Level      int    `json:"level"`
	LevelName  string `json:"level_name,omitempty"`
	LevelIcon  string `json:"level_icon,omitempty"`
	MaxLevel   bool   `json:"max_level,omitempty"`
	Total      int    `json:"total"`
	FirstVisit bool   `json:"first_visit,omitempty"`
}

// ListLevelTasks 拉取等级任务列表。响应形如 data.tasks[]。
func (c *Client) ListLevelTasks(a *auth.Auth) ([]LevelTask, error) {
	data, err := c.growthJSON(a, http.MethodGet, levelTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []LevelTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// FetchLevelProfile 拉取等级进度。
func (c *Client) FetchLevelProfile(a *auth.Auth) (*LevelProfile, error) {
	data, err := c.growthJSON(a, http.MethodGet, levelProfilePath, nil)
	if err != nil {
		return nil, err
	}
	var p LevelProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
