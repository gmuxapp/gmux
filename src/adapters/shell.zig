// Package adapters contains all built-in adapter implementations.
const std = @import("std");
const allocator = std.heap.c_allocator;
const adapter = @import("../adapter/adapter.zig");

/// All contains instances of all non-fallback adapters.
var All: std.ArrayList(*AdapterBase) = undefined;

/// DefaultFallback returns the shell adapter (always the fallback).
pub fn defaultFallback() *Shell {
    return shellAdapter;
}

/// AllAdapters returns all adapters including the default fallback.
pub fn allAdapters() []const *AdapterBase {
    return All.items;
}

/// FindByKind returns the adapter with the given name, or null if not found.
pub fn findByKind(name: []const u8) ?*AdapterBase {
    for (All.items) |a| {
        if (std.mem.eql(u8, a.name(), name)) {
            return a;
        }
    }
    if (std.mem.eql(u8, name, "shell")) {
        return shellAdapter;
    }
    return null;
}

/// AdapterBase is the base type for all adapters.
pub const AdapterBase = struct {
    pub fn name(self: *const AdapterBase) []const u8 {
        _ = self;
        @compileError("must be implemented by subclass");
    }

    pub fn discover(self: *const AdapterBase) bool {
        _ = self;
        @compileError("must be implemented by subclass");
    }

    pub fn match_(self: *const AdapterBase, command: []const []const u8) bool {
        _ = self;
        _ = command;
        @compileError("must be implemented by subclass");
    }

    pub fn env(self: *const AdapterBase, ctx: adapter.EnvContext) ?[][]const u8 {
        _ = self;
        _ = ctx;
        return null;
    }

    pub fn monitor(self: *const AdapterBase, output: []const u8) ?adapter.Event {
        _ = self;
        _ = output;
        return null;
    }
};

/// Shell is the fallback adapter. It matches all commands and parses
/// OSC 0/2 title sequences for live sidebar titles.
pub const Shell = struct {
    base: AdapterBase,

    pub fn name(self: *const Shell) []const u8 {
        _ = self;
        return "shell";
    }

    pub fn discover(self: *const Shell) bool {
        _ = self;
        return true;
    }

    pub fn match_(self: *const Shell, command: []const []const u8) bool {
        _ = self;
        _ = command;
        return true;
    }

    pub fn env(self: *const Shell, ctx: adapter.EnvContext) ?[][]const u8 {
        _ = self;
        _ = ctx;
        return null;
    }

    /// CommandTitle shows the full command with args.
    pub fn commandTitle(command: []const []const u8) []const u8 {
        if (command.len == 0) return "shell";
        const base = adapter.baseName(command[0]);
        if (command.len > 1) {
            return std.mem.join(allocator, " ", command[0..]) catch base;
        }
        return base;
    }

    pub fn launchers(self: *const Shell) []const adapter.Launcher {
        _ = self;
        return &.{
            .{
                .id = "shell",
                .label = "Shell",
                .command = &.{},
                .description = "Launch a shell",
                .available = true,
            },
        };
    }

    /// ParseOSCTitle extracts OSC 0 or OSC 2 title sequences from PTY output.
    pub fn parseOSCTitle(output: []const u8) ?[]const u8 {
        // Look for OSC (Operating System Command) sequences: ESC ] 0; ... BEL
        // or ESC ] 2; ... BEL
        var i: usize = 0;
        while (i < output.len) {
            // Look for ESC ]
            if (output[i] == 0x1b and i + 1 < output.len and output[i + 1] == ']') {
                i += 2;
                // Check for 0; or 2;
                if (i < output.len and (output[i] == '0' or output[i] == '2')) {
                    i += 1;
                    if (i < output.len and output[i] == ';') {
                        i += 1;
                        // Read until BEL (0x07) or ESC \
                        const start = i;
                        while (i < output.len) {
                            if (output[i] == 0x07) {
                                // BEL - end of sequence
                                return output[start..i];
                            }
                            if (output[i] == 0x1b and i + 1 < output.len and output[i + 1] == '\\') {
                                // ESC \ - end of sequence
                                return output[start..i];
                            }
                            i += 1;
                        }
                    }
                }
            } else {
                i += 1;
            }
        }
        return null;
    }
};

var shellAdapter: *Shell = undefined;

pub fn init() void {
    All = std.ArrayList(*AdapterBase).init(allocator);
    shellAdapter = allocator.create(Shell) catch unreachable;
    shellAdapter.* = Shell{ .base = .{} };
}
