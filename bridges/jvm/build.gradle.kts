// SPDX-License-Identifier: Apache-2.0
plugins {
    id("com.google.protobuf") version "0.9.4" apply false
    id("com.gradleup.shadow") version "8.3.5" apply false
}

allprojects {
    group = "io.nurtuo.bridge"
    // Bridge version is independent of the platform version; see README "Versioning".
    version = "0.1.0-SNAPSHOT"
    repositories { mavenCentral() }
}

subprojects {
    apply(plugin = "java-library")

    extensions.configure<JavaPluginExtension> {
        withSourcesJar()
    }
    tasks.withType<JavaCompile>().configureEach {
        options.release.set(17)
        options.encoding = "UTF-8"
        options.compilerArgs.addAll(listOf("-Xlint:all", "-Xlint:-serial", "-Xlint:-processing"))
    }
    tasks.withType<Test>().configureEach {
        useJUnitPlatform()
        testLogging {
            events("failed", "skipped")
            showStandardStreams = false
        }
    }
}
