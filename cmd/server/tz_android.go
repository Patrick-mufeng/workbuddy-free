//go:build android

// tz_android.go Android 目标专属的时区修正。
//
// 为什么需要：Go 标准库在 Android 上把 time.Local 硬编码为 UTC——
// time/zoneinfo_android.go 的 initLocal() 直接 localLoc = *UTC，既不读 $TZ 也不读
// 系统时区（标准库里这仍是个 TODO）。而本项目多处依赖 time.Local：
//   - scheduler.nextFire           schedule.*_hours 的小时判定（签到 9/21 点等）
//   - pool.CooldownUntilTomorrow4AM 硬冷却到次日 04:00
//   - server 请求日志的 HH:MM:SS
// 不修正的后果是「签到在 UTC 9 点触发」，即北京时间 17 点，五类定时任务整体偏移 8 小时，
// 请求日志时间戳也全是 UTC。
//
// 修正：进程启动最早阶段（init）把 time.Local 换成设备时区——Kotlin 侧以 TZ 环境变量
// 传入 TimeZone.getDefault().id；取不到就回落 Asia/Shanghai（与上游自然日口径一致，
// 中国无夏令时，固定 +8 与项目其余处 FixedZone("CST") 的用法等价）。
//
// 关于 tzdata 兜底：设备上 /system/usr/share/zoneinfo/tzdata 正常可读，正常路径下
// 用不到嵌入副本；保留它是为了极少数读取失败（定制 ROM / 权限异常）时 LoadLocation
// 仍能解析出真实时区，而不是静默退化成 UTC。代价约 370KB，仅 Android 产物承担。
package main

import (
	"os"
	"time"
	_ "time/tzdata"
)

func init() {
	if loc := resolveLocalLocation(); loc != nil {
		time.Local = loc
	}
}

// resolveLocalLocation 依次尝试：$TZ 指定时区 → Asia/Shanghai → 固定 +8 兜底。
// 返回 nil 表示保持标准库默认（UTC），仅在全部路径都失败时发生。
func resolveLocalLocation() *time.Location {
	if name := os.Getenv("TZ"); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}
