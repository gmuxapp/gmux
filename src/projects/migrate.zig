// Package projects handles project migration.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// migrate migrates projects from old format to new format.
pub fn migrate(allocator: Allocator) !void {
    _ = allocator;
    // In real implementation, this would migrate project data
}
