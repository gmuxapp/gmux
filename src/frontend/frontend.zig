// Package frontend handles frontend configuration.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Theme represents a frontend theme.
pub const Theme = struct {
    name: []const u8,
    colors: []const u8,
};

/// Settings represents frontend settings.
pub const Settings = struct {
    theme: []const u8,
};

/// loadTheme loads the frontend theme.
pub fn loadTheme(allocator: Allocator) !Theme {
    _ = allocator;
    return Theme{
        .name = "dark",
        .colors = "",
    };
}

/// loadSettings loads frontend settings.
pub fn loadSettings(allocator: Allocator) !Settings {
    _ = allocator;
    return Settings{
        .theme = "dark",
    };
}
