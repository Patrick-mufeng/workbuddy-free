plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.workbuddy.free"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.workbuddy.free"
        minSdk = 26
        targetSdk = 34
        versionCode = 1
        versionName = "1.0.0"

        // 只出 arm64-v8a：Go 二进制按 ABI 单独交叉编译，收窄 ABI 可避免为每个
        // 架构都塞一份 ~10MB 的副本。手机实测为 arm64-v8a（ro.product.cpu.abi）。
        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    // ⚠️ 关键配置：必须让 AGP 把 jniLibs 里的文件真正「解压落盘」到 nativeLibraryDir。
    // AGP 3.6+ 默认 extractNativeLibs=false（原生库以未压缩形式留在 APK 内由链接器
    // 直接映射），那样 nativeLibraryDir 里没有实体文件可执行；Go 网关要求该路径存在且可执行。
    // 打开 useLegacyPackaging 后，安装器会把 lib/arm64-v8a/*.so 解压到
    // /data/app/<pkg>/lib/arm64-v8a/ 并 chmod 0755——该目录属 apk_data_file，
    // SELinux 允许所有应用执行，且不受 Android 10 起对私有目录的 W^X 限制。
    packaging {
        jniLibs {
            useLegacyPackaging = true
            // libwb2api.so 其实是 Go 可执行文件而非动态库，NDK 的 strip 工具会
            // 把它当普通 .so 处理并报「Unable to strip」。Go 构建时已用 -s -w 去过符号，
            // 这里显式排除，避免每次构建刷警告。
            keepDebugSymbols += "**/libwb2api.so"
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            isShrinkResources = false
            // 自签调试证书：本项目自用分发，不做商店签名流程。
            signingConfig = signingConfigs.getByName("debug")
        }
        debug {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        viewBinding = false
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")
}
