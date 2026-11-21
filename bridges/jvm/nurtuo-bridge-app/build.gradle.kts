// SPDX-License-Identifier: Apache-2.0
//
// Packaging: one thin fat jar (SDK + grpc + connectors, no customer/vendor jars) and a zip with
// launcher scripts and an empty plugins/ directory.
//
//   gradle :nurtuo-bridge-app:shadowJar     -> build/libs/nurtuo-bridge-jvm-<version>-all.jar
//   gradle :nurtuo-bridge-app:bridgeDist    -> build/distributions/nurtuo-bridge-jvm-<version>.zip
//   gradle ... -PwithExample                -> also bundle the example connector (dev/contract testing)
plugins {
    `java-library`
    id("com.gradleup.shadow")
}

dependencies {
    implementation(project(":nurtuo-bridge-sdk"))
    runtimeOnly(project(":nurtuo-connector-jms"))
    if (providers.gradleProperty("withExample").isPresent) {
        runtimeOnly(project(":nurtuo-connector-example"))
    }
    runtimeOnly(libs.slf4j.simple) // Artemis client logs through slf4j; keep it on stderr
}

tasks.shadowJar {
    archiveBaseName.set("nurtuo-bridge-jvm")
    archiveClassifier.set("all")
    mergeServiceFiles() // META-INF/services/io.nurtuo.bridge.sdk.Connector from every connector
    exclude("META-INF/*.SF", "META-INF/*.DSA", "META-INF/*.RSA", "module-info.class")
    manifest {
        attributes(
            "Main-Class" to "io.nurtuo.bridge.sdk.BridgeMain",
            "Implementation-Title" to "nurtuo-bridge-jvm",
            "Implementation-Version" to project.version,
            "Nurtuo-Connector-Contract" to "nurtuo.connector.v1",
        )
    }
}

tasks.register<Zip>("bridgeDist") {
    group = "distribution"
    description = "Fat jar + launcher scripts + empty plugins/ directory."
    archiveFileName.set("nurtuo-bridge-jvm-${project.version}.zip")
    destinationDirectory.set(layout.buildDirectory.dir("distributions"))
    from(tasks.shadowJar) {
        into("lib")
        rename { "nurtuo-bridge.jar" }
    }
    from(rootProject.file("scripts")) {
        into("bin")
        filePermissions { unix("rwxr-xr-x") }
    }
    from(file("plugins-README.txt")) {
        into("plugins")
        rename { "README.txt" }
    }
}

// The jar is the build output; the plain (non-shadow) jar of this module is empty and not useful.
tasks.jar { enabled = false }
tasks.assemble { dependsOn(tasks.shadowJar) }
