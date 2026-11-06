plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Release signing comes from GitHub secrets (see .github/workflows/android.yml).
val keystorePath: String? = System.getenv("TGVAULT_KEYSTORE")

// The library app, for sharing one folder of the backup with someone, is this
// same code built with -PlibraryFolder=<folder of the backup>, e.g. Movies. It
// installs next to the normal app and can only browse and download that folder.
val libraryFolder: String = (findProperty("libraryFolder") as String?) ?: ""

android {
    namespace = "io.github.sammy77a.tgvault"
    compileSdk = 35

    defaultConfig {
        applicationId = "io.github.sammy77a.tgvault" + if (libraryFolder.isEmpty()) "" else ".library"
        buildConfigField("String", "LIBRARY_FOLDER", "\"$libraryFolder\"")
        // Service keys for the movie app, from the build's environment (GitHub secrets); never in the repository.
        buildConfigField("String", "TMDB_KEY", "\"${System.getenv("TMDB_KEY") ?: ""}\"")
        buildConfigField("String", "SUBTITLE_KEY", "\"${System.getenv("SUBTITLE_KEY") ?: ""}\"")
        manifestPlaceholders["appLabel"] = if (libraryFolder.isEmpty()) "tgvault" else libraryFolder.substringAfterLast('/')
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

    buildFeatures {
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    lint {
        // Don't let lint warnings block building the APK.
        checkReleaseBuilds = false
    }
}

dependencies {
    // The Go code, built by `gomobile bind` into app/libs.
    implementation(fileTree(mapOf("dir" to "libs", "include" to listOf("*.aar"))))
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.work:work-runtime-ktx:2.10.0")
    implementation("androidx.swiperefreshlayout:swiperefreshlayout:1.1.0")
    // Plays videos the WebView can't (MKV, x265/HEVC) with the phone's decoders.
    implementation("androidx.media3:media3-exoplayer:1.5.1")
    implementation("androidx.media3:media3-ui:1.5.1")
}
