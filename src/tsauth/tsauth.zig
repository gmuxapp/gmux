// Package tsauth handles Tailscale authentication.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Listener wraps a TCP listener with Tailscale auth.
pub const Listener = struct {
    allocator: Allocator,
    addr: []const u8,

    pub fn init(allocator: Allocator, addr: []const u8) !*Listener {
        const self = try allocator.create(Listener);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.addr = try allocator.dupe(u8, addr);

        return self;
    }

    pub fn deinit(self: *Listener) void {
        self.allocator.free(self.addr);
        self.allocator.destroy(self);
    }

    /// diag returns diagnostic info.
    pub fn diag(self: *Listener) struct { fqdn: []const u8 } {
        _ = self;
        return .{ .fqdn = "" };
    }
};
