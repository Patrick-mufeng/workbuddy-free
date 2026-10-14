# WorkBuddy 网关 · Android App

把 [workbuddy-free](../README.md) 网关整体封装成安卓应用：**手机本体当服务器**，息屏后台常驻，
局域网内其它设备直接接入 OpenAI 兼容接口。

## 架构

```
APK (Kotlin)
├── MainActivity      WebView 加载 http://127.0.0.1:3012/panel/（内嵌管理面板）
├── GatewayService    前台服务（specialUse）拉起并守护 Go 网关进程
├── BootReceiver      开机自启（受 MIUI 等 ROM 的自启动白名单限制，见下）
└── app/src/main/jniLibs/arm64-v8a/libwb2api.so
                      ← Go 网关二进制，以「原生库」名义随 APK 分发
```

Go 网关的凭证（`auths/`）、状态（`data/`）、配置（`config.json`）都落在应用私有目录，
**卸载即随应用清除**，不需要任何存储权限。

## 三个必须处理的平台差异

### 1. 二进制必须以 `lib*.so` 放进 jniLibs

Android 10 起，targetSdk ≥ 29 的应用**禁止 exec 私有目录**（`filesDir` / `cacheDir`）里的文件——
这是 W^X 策略。而 APK 中以 `lib` 开头、`.so` 结尾的原生库会被安装到
`nativeLibraryDir`（`/data/app/<pkg>/lib/arm64/`），该目录属 SELinux 的 `apk_data_file`，
对所有应用域开放执行权限，安装器还会 `chmod 0755`。

因此流程是：Go 二进制命名为 `libwb2api.so` → 放进 `jniLibs/arm64-v8a/` →
**`packaging { jniLibs { useLegacyPackaging = true } }`**（AGP 3.6+ 默认不解压，
不打开这个开关 `nativeLibraryDir` 里就没有实体文件）→ 从 `applicationInfo.nativeLibraryDir` 启动。

（shadowsocks-android / Orbot / ConnectBot / VLC 等成熟应用同此做法。）

### 2. 必须用 cgo 构建（CGO_ENABLED=1）

Go 标准库的纯 Go DNS 解析器**在 Android 上不可用**。`net/conf.go` 里写得很直白：

> `// DNS requests don't work on Android, so prefer the cgo resolver. // Issue #10714.`

Android 没有 `/etc/resolv.conf`（DNS 走 netd 的 socket 代理），纯 Go 解析器读不到配置就回落到
`127.0.0.1:53`，而那里没有 DNS 服务——结果是网关能启动、面板能打开，但**所有到
`copilot.tencent.com` 的请求全部解析失败**。

开 cgo 后走 bionic 的 `getaddrinfo` → netd，DNS 正常；同时 `x509` 会带上
`/system/etc/security/cacerts`（Android 系统根证书），HTTPS 才可信。

### 3. 必须修正时区（见 `cmd/server/tz_android.go`）

Go 标准库在 Android 上把 `time.Local` **硬编码为 UTC**——`time/zoneinfo_android.go` 的
`initLocal()` 直接 `localLoc = *UTC`，既不读 `$TZ` 也不读系统时区（标准库里这仍是个 TODO）。

而本项目多处依赖 `time.Local`：签到/旅行等定时任务的小时判定、硬冷却到次日 04:00、
请求日志时间戳。不修正的话，`schedule.checkin_hours: [9, 21]` 会在 UTC 9 点触发，
即**北京时间 17 点**，五类定时任务整体偏移 8 小时。

修正方式：`cmd/server/tz_android.go`（`//go:build android`）在 `init` 阶段用
`$TZ` 环境变量还原设备时区；`GatewayService` 启动子进程时把
`TimeZone.getDefault().id` 传进环境。取不到时回落固定 +8（与项目其余处
`FixedZone("CST", 8*3600)` 的口径一致）。

> 该文件额外空白导入 `time/tzdata`（约 370KB）作为兜底：极少数定制 ROM 上
> `/system/usr/share/zoneinfo/tzdata` 不可读时，`LoadLocation` 仍能解析出真实时区，
> 而不是静默退化成 UTC。仅 Android 产物承担这个体积。

## 界面

面板是**手机优先的响应式前端**，同一套 DOM 在两种形态间自适应：

| 视口 | 布局 |
|---|---|
| 手机（< 620px） | 顶部固定头条 + **底部标签栏**（拇指可达），账号用**卡片**（一屏读完一个账号，无需横向滚动），观测条 2 列 |
| 平板（620–980px） | 卡片双列，观测条 3 列 |
| 桌面（≥ 980px） | **左侧栏**导航，卡片 2–3 列自适应网格，观测条一行 6 格 |

触控细节：操作按钮最小高度 34px（批量动作 36px），图表点选走 `pointerdown`（手指按下即出读数，不依赖悬停），底部导航让开系统手势条（`env(safe-area-inset-bottom)`）。

## 从其它设备访问

不需要额外配置，网关本来就监听 `0.0.0.0`：

1. **在手机面板上点「复制访问地址」**，拿到 `http://<手机IP>:3012`（面板顶部的「局域网地址」也一直显示着 `.../v1` 的接口地址）。
2. 在电脑 / 平板的浏览器里打开这个地址——根路径会**自动跳到面板**，不用记 `/panel/` 后缀。
3. 输入 `api_key` 进入，之后就能像在手机上一样查看账号、签到、添加账号、看统计。

同地址也能直接当 OpenAI 客户端用：Base URL 填 `http://<手机IP>:3012/v1`。

> **不是所有设备都能连上**：确认手机与对方在同一 Wi-Fi，且手机没有开「AP 隔离」的热点模式。
> 反过来，**手机接公共 Wi-Fi 时 3012 端口对同网段完全开放**——请确保 `api_key` 非空。

## 构建

需要：**Go ≥ 1.22**、**Android NDK r28+**（r28 起默认满足 16KB 页对齐）、**JDK 17**、**Android SDK**。

```bash
cd android
./build.sh              # 构建 debug APK
./build.sh release      # 构建 release APK
./build.sh --install    # 构建并 adb 安装到已连接设备
```

脚本会自动探测工具链位置，可用 `ANDROID_NDK_HOME` / `ANDROID_HOME` / `JAVA_HOME` 覆盖。

产物：`android/app/build/outputs/apk/debug/app-debug.apk`（约 14 MB）。

## 使用

1. 安装后打开应用，首次会自动拉起网关，并引导**关闭电池优化**（Doze 会挂起网络访问，
   不加白名单定时任务与转发都会静默失效）。
2. 面板即内嵌的 Web 管理面板。点「添加账号」→ 复制授权链接 → 浏览器完成登录 →
   自动检测并落盘凭证，**无需重启**。
3. 顶部显示**局域网接入地址**（如 `http://192.168.31.145:3012/v1`），点击即复制，
   填到其它设备的 Base URL 即可接入。
4. 面板 API 密钥在「配置」页查看（首次启动随机生成，记录在 `files/config.json`）。

## 已知限制

- **MIUI / HyperOS 等 ROM 会拦截开机自启**：日志里是
  `BroadcastQueueInjector: Unable to launch ... : process is not permitted to auto start`，
  即 MIUI 自启动白名单拦截，应用侧无法绕过。需要用户手动设置：

  > 设置 → 应用设置 → 应用管理 → WorkBuddy 网关 → **自启动** 打开；
  > 同时把**省电策略**设为「无限制」。

  开启后开机自启生效。其它 ROM（原生 / 三星 / 一加等）一般无需额外设置。

- **通知权限**：Android 13+ 首次启动会请求通知权限；拒绝后前台服务仍可运行，
  但通知不可见，被系统回收的概率会升高。

- **长时间运行**：7×24 常驻建议插电使用，持续运行会增加耗电与发热。

- **仅 arm64-v8a**：Go 二进制按 ABI 单独交叉编译，当前只打包 arm64-v8a
  （覆盖绝大多数现代安卓手机）。需要 32 位或 x86 模拟器时，改
  `app/build.gradle.kts` 的 `abiFilters` 并在 `build.sh` 里加对应 `GOARCH`。

- **安全**：应用已设 `usesCleartextTraffic="true"`（网关是明文 HTTP）。手机接入公共 Wi-Fi 时，
  3012 端口对同网段完全开放——**务必先在面板「配置」页设置 `api_key`**
  （首次启动已自动生成一个随机密钥）。

## 安全与合规

与原项目一致：仅限**本人授权账号、本机 / 私有环境**使用，遵守 WorkBuddy 服务条款与所在地法律。
打包成 APK 分发会放大条款风险，请自行评估。
