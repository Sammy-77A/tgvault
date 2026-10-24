plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Release signing comes from GitHub secrets (see .github/workflows/android.yml).
val keystorePath: String? = System.getenv("TGVAULT_KEYSTORE")

android {
    namespace = "io.github.sammy77a.tgvault"
    compileSdk = 35

    defaultConfig {
        applicationId = "io.github.sammy77a.tgvault"
        minSdk = 26
        targetSdk = 35
        versionCode = (System.getenv("TGVAULT_VERSION_CODE") ?: "1").toInt()
        versionName = System.getenv("TGVAULT_VERSION_NAME") ?: "0.2.0-dev"
    }

    signingConfigs {
        if (keystorePath != null) {
            create("release") {
                storeFile = file(keystorePath)
                storeType = "pkcs12"
                storePassword = System.getenv("TGVAULT_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("TGVAULT_KEY_ALIAS")
                keyPassword = System.getenv("TGVAULT_KEYSTORE_PASSWORD")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            if (keystorePath != null) signingConfig = signingConfigs.getByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    // The Go code, built by `gomobile bind` into app/libs.
    implementation(fileTree(mapOf("dir" to "libs", "include" to listOf("*.aar"))))
}
