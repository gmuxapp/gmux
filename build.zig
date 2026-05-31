const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // http.zig dependency
    const httpz_dep = b.dependency("httpz", .{
        .target = target,
        .optimize = optimize,
    });

    // Shared module for common packages
    const lib_mod = b.addModule("gmux_lib", .{
        .root_source_file = b.path("src/root.zig"),
        .target = target,
        .link_libc = true,
    });

    // Add http.zig as import (module name is httpz)
    lib_mod.addImport("httpz", httpz_dep.module("httpz"));

    // gmux CLI binary
    const gmux_exe = b.addExecutable(.{
        .name = "gmux",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/gmux/main.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .imports = &.{
                .{ .name = "gmux_lib", .module = lib_mod },
            },
        }),
    });
    b.installArtifact(gmux_exe);

    // gmuxd daemon binary
    const gmuxd_exe = b.addExecutable(.{
        .name = "gmuxd",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/gmuxd/main.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .imports = &.{
                .{ .name = "gmux_lib", .module = lib_mod },
            },
        }),
    });
    b.installArtifact(gmuxd_exe);

    // Tests
    const lib_tests = b.addTest(.{
        .root_module = lib_mod,
    });
    const run_lib_tests = b.addRunArtifact(lib_tests);

    const test_step = b.step("test", "Run unit tests");
    test_step.dependOn(&run_lib_tests.step);

    // Run steps
    const run_gmux_step = b.step("run-gmux", "Run gmux CLI");
    const run_gmux = b.addRunArtifact(gmux_exe);
    run_gmux_step.dependOn(&run_gmux.step);
    if (b.args) |args| run_gmux.addArgs(args);

    const run_gmuxd_step = b.step("run-gmuxd", "Run gmuxd daemon");
    const run_gmuxd = b.addRunArtifact(gmuxd_exe);
    run_gmuxd_step.dependOn(&run_gmuxd.step);
    if (b.args) |args| run_gmuxd.addArgs(args);
}
