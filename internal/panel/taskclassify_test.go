package panel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestClassifyTaskFourGroups 四分类的核心判据。
//
// 设计依据（2026-11 实测）：9 个国内账号的成长任务列表只剩 4 个码，
// 其中 wb_wechat_oa_subscribe_task 带 valid_end 且无自动动作。旧实现
// 只按「码在不在 autoActions 白名单」判断，会把它静默丢弃——面板因此
// 显示「没有待办」，而用户实际有一项 7 天后到期、值 50 分的任务没做。
func TestClassifyTaskFourGroups(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		task upstream.Task
		want taskCapability
	}{
		{
			name: "有动作未完成 → 可自动",
			task: upstream.Task{TaskCode: "RichMeow_Chat", Target: 1, Current: 0},
			want: capAuto,
		},
		{
			name: "有动作且达标未领 → 待领奖（不是跳过）",
			// 这一条是旧 growthPending 的 bug：它 return false 把这类任务排除了，
			// 注释却写「也入队」。积分卡住不动的主因之一。
			task: upstream.Task{TaskCode: "RichMeow_Chat", Target: 1, Current: 1},
			want: capClaim,
		},
		{
			name: "无动作但上游下发 → 需人工（不能静默丢弃）",
			task: upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Target: 1, Current: 0},
			want: capManual,
		},
		{
			name: "已过 valid_end → 已过期",
			task: upstream.Task{
				TaskCode: "wb_wechat_oa_subscribe_task", Target: 1, Current: 0,
				ValidEnd: "2026-11-20T23:59:00+08:00",
			},
			want: capExpired,
		},
		{
			name: "未到 valid_end → 按正常分类（需人工）",
			task: upstream.Task{
				TaskCode: "wb_wechat_oa_subscribe_task", Target: 1, Current: 0,
				ValidEnd: "2026-11-30T23:59:00+08:00",
			},
			want: capManual,
		},
		{
			name: "已领取 → 待领奖组（调用方据 Claimed 再跳过）",
			task: upstream.Task{TaskCode: "Library_read", Target: 1, Current: 1, AcceptStatus: "claimed", Claimed: true},
			want: capClaim,
		},
		{
			name: "上游锁定 → 需人工（不进队列空跑）",
			task: upstream.Task{TaskCode: "RichMeow_Chat", Target: 1, Current: 0, Locked: true},
			want: capManual,
		},
		{
			name: "过期优先于是否有动作（过期任务即使有动作也不该跑）",
			task: upstream.Task{
				TaskCode: "RichMeow_Chat", Target: 1, Current: 0,
				ValidEnd: "2026-01-01T00:00:00+08:00",
			},
			want: capExpired,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTask(c.task, now); got != c.want {
				t.Fatalf("classifyTask(%s) = %v, want %v", c.task.TaskCode, got, c.want)
			}
		})
	}
}

// TestGrowthPendingClaimsDoneButUnclaimed growthPending 必须只认「可自动」档：
// 达标未领不属于它（走 claim_only），已领取也不属于。
func TestGrowthPendingClaimsDoneButUnclaimed(t *testing.T) {
	done := upstream.Task{TaskCode: "RichMeow_Chat", Target: 1, Current: 1}
	if growthPending(done) {
		t.Fatal("达标未领的任务不应被 growthPending 收为可自动（应走 claim_only）")
	}
	todo := upstream.Task{TaskCode: "RichMeow_Chat", Target: 1, Current: 0}
	if !growthPending(todo) {
		t.Fatal("未完成且有动作的任务应可自动")
	}
	unknown := upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Target: 1, Current: 0}
	if growthPending(unknown) {
		t.Fatal("无动作的任务不应被收为可自动")
	}
}

// TestTaskExpiredBoundaries Expired 的边界与容错。
func TestTaskExpiredBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		end  string
		want bool
	}{
		{"空 = 永久，不过期", "", false},
		{"已过", "2026-11-22T23:59:00+08:00", true},
		{"未到", "2026-11-30T23:59:00+08:00", false},
		// 解析失败按未过期处理：宁可多展示一项，也不要因上游格式变更
		// 把所有任务静默藏掉。
		{"格式非法按未过期", "not-a-timestamp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := upstream.Task{TaskCode: "x", ValidEnd: c.end}
			if got := task.Expired(now); got != c.want {
				t.Fatalf("Expired(%q) = %v, want %v", c.end, got, c.want)
			}
		})
	}
}

// TestManualHintCoversKnownTasks 需人工任务必须给出可操作的说明，
// 而不是一句笼统的「需客户端交互」。
func TestManualHintCoversKnownTasks(t *testing.T) {
	sub := manualHint(upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task"})
	if sub == "" || sub == "无自动化动作，请在官方客户端按任务说明完成" {
		t.Fatalf("公众号任务应给出具体指引，got %q", sub)
	}
	// 未知任务也要有兜底文案（带上跳转地址，便于用户找到入口）。
	other := manualHint(upstream.Task{TaskCode: "brand_new_task", JumpURL: "workbuddy://chat"})
	if other == "" {
		t.Fatal("未知任务也应有兜底提示")
	}
}

// TestScanTaskViewCarriesHint JSON 序列化必须把 hint 透出去。
//
// 这条守的是前后端契约：前端用 t.hint 展示「怎么做」，但 upstream.Task 本身
// 没有 hint 字段——早先版本前端读 t.hint 恒为 undefined，行尾空白，
// 用户看不出为什么这个任务不能一键完成。scanTaskView 嵌 Task + 附加 Hint
// 解决，嵌入字段在 JSON 里是平铺的（服务端下发的 task_code/title 等照旧）。
func TestScanTaskViewCarriesHint(t *testing.T) {
	view := scanTaskView{
		Task: upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task", Title: "关注公众号"},
		Hint: manualHint(upstream.Task{TaskCode: "wb_wechat_oa_subscribe_task"}),
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["task_code"] != "wb_wechat_oa_subscribe_task" {
		t.Fatalf("嵌套字段应平铺到 JSON 顶层，got %v", decoded)
	}
	hint, _ := decoded["hint"].(string)
	if hint == "" {
		t.Fatal("hint 必须透出，否则前端无从展示「需人工」的具体做法")
	}
}
