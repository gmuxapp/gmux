// Package gmux provides the CLI setup.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// CLI is the CLI application.
pub const CLI = struct {
    allocator: Allocator,
    args: []const []const u8,

    pub fn init(allocator: Allocator, args: []const []const u8) !*CLI {
        const self = try allocator.create(CLI);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.args = args;

        return self;
    }

    pub fn deinit(self: *CLI) void {
        self.allocator.destroy(self);
    }

    /// run runs the CLI.
    pub fn run(self: *CLI) !void {
        _ = self;
        // In real implementation, this would parse args and dispatch
    }
};
