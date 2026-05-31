// Package adapter provides the adapter registry.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("adapter.zig");

/// Registry holds all registered adapters.
pub const Registry = struct {
    allocator: Allocator,
    adapters: std.StringHashMap(*adapter.AdapterBase),

    pub fn init(allocator: Allocator) !*Registry {
        const self = try allocator.create(Registry);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.adapters = std.StringHashMap(*adapter.AdapterBase).init(allocator);

        return self;
    }

    pub fn deinit(self: *Registry) void {
        self.adapters.deinit();
        self.allocator.destroy(self);
    }

    /// register registers an adapter.
    pub fn register(self: *Registry, name: []const u8, a: *adapter.AdapterBase) !void {
        try self.adapters.put(name, a);
    }

    /// get retrieves an adapter by name.
    pub fn get(self: *Registry, name: []const u8) ?*adapter.AdapterBase {
        return self.adapters.get(name);
    }

    /// all returns all registered adapters.
    pub fn all(self: *Registry) []const *adapter.AdapterBase {
        return self.adapters.values();
    }
};
