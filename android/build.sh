#!/usr/bin/env bash
# build.sh 构建工作 buddy-free 安卓 APK。
#
# 两步走：
#   1) 用 NDK 的 clang 以 cgo 交叉编译 Go 网关（GOOS=android），产出 libwb2api.so
#   2) 交给 Gradle 打包 APK
#
# 为什么必须用 cgo（CGO_ENABLED=1）：
#   Go 标准库的纯 Go DNS 解析器在 Android 上不可用——net/conf.go 里写得很明确：
#   "DNS requests don't work on Android, so prefer the cgo resolver"（golang/go#10714）。
#   Android 没有 /etc/resolv.conf（DNS 走 netd 的 socket 代理），纯 Go 解析器读不到
#   配置就回落到 127.0.0.1:53，而那里没有 DNS 服务，结果所有上游域名解析全部失败。
#   开 cgo 后走 bionic 的 getaddrinfo → netd，DNS 正常；同时 x509 会带上
#   /system/etc/security/cacerts（Android 系统根证书），HTTPS 才可信。
#
# 用法：
#   ./build.sh                 # 构建 debug APK
#   ./build.sh release         # 构建 release APK
#   ./build.sh --install       # 构建后直接 adb 安装到已连接设备
#
# 依赖（可用环境变量覆盖）：
#   ANDROID_NDK_HOME   NDK 根目录（默认探测常见位置）
#   ANDROID_HOME       Android SDK 根目录（Gradle 需要）
#   JAVA_HOME          JDK 17

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BUILD_TYPE="debug"
DO_INSTALL=0
for arg in "$@"; do
  case "$arg" in
    release) BUILD_TYPE="release" ;;
    debug) BUILD_TYPE="debug" ;;
    --install) DO_INSTALL=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "未知参数：$arg" >&2; exit 2 ;;
  esac
done

# ── 工具链探测 ────────────────────────────────────────────────────────
detect_sdk() {
  for d in "${ANDROID_HOME:-}" "${ANDROID_SDK_ROOT:-}" \
           "$HOME/AppData/Local/Android/Sdk" "/d/android-sdk" "/c/Android/Sdk"; do
    [ -n "$d" ] && [ -d "$d/platforms" ] && { echo "$d"; return; }
  done
}
detect_ndk() {
  for d in "${ANDROID_NDK_HOME:-}" "${NDK_HOME:-}" \
           "/d/android-tools/android-ndk-r28b" "$HOME/AppData/Local/Android/Sdk/ndk"/*; do
    [ -n "$d" ] && [ -f "$d/source.properties" ] && { echo "$d"; return; }
  done
}
detect_jdk() {
  for d in "${JAVA_HOME:-}" "/d/android-tools/jdk-17.0.20.1+1" \
           "/c/Program Files/Java"/* "$HOME/AppData/Local/Android/JDK"/*; do
    [ -n "$d" ] && [ -x "$d/bin/java" -o -x "$d/bin/java.exe" ] && { echo "$d"; return; }
  done
}

SDK="$(detect_sdk || true)"
NDK="$(detect_ndk || true)"
JDK="$(detect_jdk || true)"

[ -z "$SDK" ] && { echo "✗ 未找到 Android SDK，请设置 ANDROID_HOME" >&2; exit 1; }
[ -z "$NDK" ] && { echo "✗ 未找到 Android NDK（需要 r28+ 以自动满足 16KB 页对齐）" >&2; exit 1; }
[ -z "$JDK" ] && { echo "✗ 未找到 JDK 17，请设置 JAVA_HOME" >&2; exit 1; }

export ANDROID_HOME="$SDK"
export ANDROID_SDK_ROOT="$SDK"
export ANDROID_NDK_HOME="$NDK"
export JAVA_HOME="$JDK"
export PATH="$JDK/bin:$SDK/platform-tools:$PATH"

echo "  SDK : $SDK"
echo "  NDK : $NDK"
echo "  JDK : $JDK"

NDK_VERSION="$(grep -m1 'Pkg.Revision' "$NDK/source.properties" 2>/dev/null | cut -d= -f2 | tr -d ' ' || true)"
echo "  NDK 版本: ${NDK_VERSION:-unknown}"

# ── 定位 NDK clang ────────────────────────────────────────────────────
HOST_TAG=""
for t in windows-x86_64 linux-x86_64 darwin-x86_64; do
  [ -d "$NDK/toolchains/llvm/prebuilt/$t" ] && { HOST_TAG="$t"; break; }
done
[ -z "$HOST_TAG" ] && { echo "✗ NDK 缺少 llvm prebuilt 工具链" >&2; exit 1; }
TOOLCHAIN="$NDK/toolchains/llvm/prebuilt/$HOST_TAG/bin"

# 定位 aarch64 的 clang wrapper。
# ⚠️ 顺序有讲究：NDK 同时提供两个版本——无扩展名的 bash 脚本，和 Windows 的 .cmd。
# Go 在 Windows 上通过 exec 直接调用 CC，无法执行 bash 脚本（没有 shebang 支持），
# 必须用 .cmd；在 Linux/macOS 上则相反。故按宿主平台选择。
ANDROID_API=26  # 与 app/build.gradle.kts 的 minSdk 一致
CLANG=""
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    for cand in "$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang.cmd" \
                "$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang.exe"; do
      [ -f "$cand" ] && { CLANG="$cand"; break; }
    done
    ;;
  *)
    for cand in "$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang" \
                "$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang.exe"; do
      [ -f "$cand" ] && { CLANG="$cand"; break; }
    done
    ;;
esac
# 指定 API 级别的 wrapper 不存在时，退而取任意 aarch64 wrapper（排除 C++）
if [ -z "$CLANG" ]; then
  CLANG="$(ls "$TOOLCHAIN"/aarch64-linux-android*-clang* 2>/dev/null | grep -v '++' | head -1 || true)"
fi
[ -z "$CLANG" ] && { echo "✗ 未找到 aarch64 NDK clang（$TOOLCHAIN）" >&2; exit 1; }

# ── 步骤 1：交叉编译 Go 网关 ───────────────────────────────────────────
JNI_DIR="$SCRIPT_DIR/app/src/main/jniLibs/arm64-v8a"
mkdir -p "$JNI_DIR"
OUT="$JNI_DIR/libwb2api.so"

echo
echo "[1/2] 交叉编译 Go 网关 → $(basename "$OUT")"
echo "      CC=$CLANG"
cd "$REPO_ROOT"
CGO_ENABLED=1 \
GOOS=android \
GOARCH=arm64 \
CC="$CLANG" \
go build -trimpath -ldflags="-s -w" -o "$OUT" ./cmd/server

SIZE_MB=$(awk "BEGIN{printf \"%.1f\", $(stat -c%s "$OUT" 2>/dev/null || stat -f%z "$OUT")/1048576}")
echo "      ✓ 产出 ${SIZE_MB} MB"

# 自检：必须是带 Android 链接器的 PIE，且链接到 bionic。
if command -v file >/dev/null 2>&1; then
  file "$OUT" | sed 's/^/      /'
fi

# 自检：确认 cgo 真的生效（否则 DNS 在手机上必挂）。
if ! grep -aq 'libc\.so' "$OUT"; then
  echo "✗ 二进制未链接 libc.so —— cgo 未生效，DNS 在 Android 上会失败。" >&2
  exit 1
fi

# ── 步骤 2：Gradle 打包 ────────────────────────────────────────────────
echo
echo "[2/2] Gradle 打包 $BUILD_TYPE APK"
cd "$SCRIPT_DIR"

GRADLE_TASK="assemble$(echo "${BUILD_TYPE:0:1}" | tr '[:lower:]' '[:upper:]')${BUILD_TYPE:1}"

# 优先用本地解压的发行版（不依赖网络下载 distribution）；其次 wrapper。
GRADLE_BIN="$(ls -d /d/android-tools/gradle-*/bin/gradle 2>/dev/null | head -1 || true)"
if [ -n "$GRADLE_BIN" ]; then
  "$GRADLE_BIN" "$GRADLE_TASK"
elif [ -x "./gradlew" ]; then
  ./gradlew "$GRADLE_TASK"
elif command -v gradle >/dev/null 2>&1; then
  gradle "$GRADLE_TASK"
else
  echo "✗ 未找到 gradle（可先运行 ./gradlew 或安装 gradle）" >&2
  exit 1
fi

APK="$(ls -t app/build/outputs/apk/"$BUILD_TYPE"/*.apk 2>/dev/null | head -1 || true)"
[ -z "$APK" ] && { echo "✗ 未找到构建产物 APK" >&2; exit 1; }

echo
echo "✓ 构建完成：$APK"
ls -lh "$APK" | awk '{print "  大小: "$5}'

# ── release 产物归档到 dist/（该目录入库，供直接下载安装）─────────────
# 只归档 release：debug 包带调试信息且体积大一倍，没有分发价值。
if [ "$BUILD_TYPE" = "release" ]; then
  VERSION="$(grep -m1 'versionName' app/build.gradle.kts | sed 's/.*"\(.*\)".*/\1/')"
  DIST="$SCRIPT_DIR/dist"
  mkdir -p "$DIST"
  TARGET="$DIST/workbuddy-gateway-${VERSION}-arm64.apk"
  cp "$APK" "$TARGET"
  # 校验文件与仓库 Release 的 SHA256SUMS.txt 约定一致，方便用户验证完整性。
  (cd "$DIST" && sha256sum "$(basename "$TARGET")" > SHA256SUMS.txt)
  echo
  echo "✓ 已归档到 dist/：$(basename "$TARGET")"
  cat "$DIST/SHA256SUMS.txt" | sed 's/^/  /'
fi

# ── 可选：安装到设备 ───────────────────────────────────────────────────
if [ "$DO_INSTALL" = "1" ]; then
  echo
  echo "安装到设备…"
  adb install -r "$APK"
  echo "✓ 已安装。应用内点「启动」，或直接打开应用（首次会自动拉起网关）。"
fi
