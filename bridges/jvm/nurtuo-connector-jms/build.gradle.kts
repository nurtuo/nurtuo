// SPDX-License-Identifier: Apache-2.0
plugins {
    `java-library`
}

dependencies {
    api(project(":nurtuo-bridge-sdk"))
    // The JMS API is the only compile-time dependency on messaging: providers (Artemis, IBM MQ, ...)
    // are loaded by class name at runtime so the bridge never has to ship a vendor client.
    api(libs.jakarta.jms.api)

    // Artemis client is bundled as the default, tested provider.
    runtimeOnly(libs.artemis.jakarta.client)
    runtimeOnly(libs.slf4j.simple)

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.platform.launcher)
    // Embedded broker for tests (in-VM transport, no network).
    testImplementation(libs.artemis.server)
    testImplementation(libs.artemis.jakarta.client)
}

tasks.test {
    // Artemis logs through slf4j-simple to stderr; keep the build output readable.
    systemProperty("org.slf4j.simpleLogger.defaultLogLevel", "warn")
}
