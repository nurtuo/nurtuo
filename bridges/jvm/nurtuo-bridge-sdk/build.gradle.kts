// SPDX-License-Identifier: Apache-2.0
plugins {
    `java-library`
    id("com.google.protobuf")
}

dependencies {
    api(libs.grpc.api)
    api(libs.grpc.stub)
    api(libs.grpc.protobuf)
    api(libs.protobuf.java)
    api(libs.grpc.services) // health service
    // In-process transport is part of the public testing harness (io.nurtuo.bridge.sdk.testing).
    api(libs.grpc.inprocess)
    // TCP / Unix domain socket transport used by BridgeServer (shaded netty, no netty conflicts
    // with customer libraries on the plugin classpath).
    implementation(libs.grpc.netty.shaded)
    compileOnly(libs.annotations.api) // @javax.annotation.Generated in generated gRPC code

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.platform.launcher)
}

// Stubs are generated from the single repo-level contract; nothing is copied or vendored.
// Layout: <repo>/proto/nurtuo/connector/v1/connector.proto, rootProject = <repo>/bridges/jvm.
val repoProtoDir = rootProject.file("../../proto")

sourceSets {
    main {
        proto {
            srcDir(repoProtoDir)
            include("nurtuo/connector/**")
        }
    }
}

protobuf {
    protoc { artifact = "com.google.protobuf:protoc:${libs.versions.protobuf.get()}" }
    plugins {
        create("grpc") { artifact = "io.grpc:protoc-gen-grpc-java:${libs.versions.grpc.get()}" }
    }
    generateProtoTasks {
        all().forEach { task ->
            task.plugins { create("grpc") }
        }
    }
}

tasks.jar {
    manifest {
        attributes(
            "Implementation-Title" to "nurtuo-bridge-sdk",
            "Implementation-Version" to project.version,
            // Contract version this SDK implements; surfaced by `BridgeMain --version`.
            "Nurtuo-Connector-Contract" to "nurtuo.connector.v1",
        )
    }
}
