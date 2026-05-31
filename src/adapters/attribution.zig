// Package attribution detects attribution in session files.
const std = @import("std");

/// Attribution detects attribution in session files.
pub const Attribution = struct {
    /// detect detects attribution in a line.
    pub fn detect(line: []const u8) bool {
        // In real implementation, this would detect attribution markers
        return std.mem.indexOf(u8, line, "gmux:") != null;
    }
};
