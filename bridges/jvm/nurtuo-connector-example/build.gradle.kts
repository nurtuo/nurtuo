// SPDX-License-Identifier: Apache-2.0
plugins {
    `java-library`
}

dependencies {
    api(project(":nurtuo-bridge-sdk"))

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly(libs.junit.platform.launcher)
}
