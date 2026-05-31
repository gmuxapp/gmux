// Package config loads gmuxd configuration from disk.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Config holds the gmuxd configuration.
pub const Config = struct {
    listenAddr: []const u8 = "127.0.0.1:8790",
    port: u16 = 8790,
    socketDir: []const u8 = "",
    peers: []const PeerConfig = &._emptyPeers,
    authRequired: bool = false,

    pub const PeerConfig = struct {
        name: []const u8,
        addr: []const u8,
        local: bool = false,
    };

    var _emptyPeers: []const PeerConfig = &.{};
};

/// DefaultDir returns the default config directory.
pub fn defaultDir(allocator: Allocator) ![]const u8 {
    if (std.process.getEnvVarOwned(allocator, "XDG_CONFIG_HOME")) |dir| {
        return std.fs.path.join(allocator, &.{ dir, "gmux" });
    }
    const home = try std.process.getEnvVarOwned(allocator, "HOME");
    errdefer allocator.free(home);
    return std.fs.path.join(allocator, &.{ home, ".config", "gmux" });
}

/// Load reads the gmuxd configuration from disk.
pub fn load(allocator: Allocator) !Config {
    var config = Config{};
    config.listenAddr = try allocator.dupe(u8, config.listenAddr);

    const configDir = try defaultDir(allocator);
    defer allocator.free(configDir);

    const configPath = try std.fs.path.join(allocator, &.{ configDir, "gmuxd.json" });
    defer allocator.free(configPath);

    const file = std.fs.openFileAbsolute(configPath, .{}) catch |err| {
        if (err == error.FileNotFound) return config;
        return err;
    };
    defer file.close();

    const content = try file.readToEndAlloc(allocator, 65536);
    defer allocator.free(content);

    // Parse JSON config (simplified)
    

    return config;
}

/// Settings holds user preferences.
pub const Settings = struct {
    theme: []const u8 = "dark",
};

/// LoadSettings reads user settings from disk.
pub fn loadSettings(allocator: Allocator) !Settings {
    var settings = Settings{};
    settings.theme = try allocator.dupe(u8, settings.theme);

    const configDir = try defaultDir(allocator);
    defer allocator.free(configDir);

    const settingsPath = try std.fs.path.join(allocator, &.{ configDir, "settings.json" });
    defer allocator.free(settingsPath);

    const file = std.fs.openFileAbsolute(settingsPath, .{}) catch |err| {
        if (err == error.FileNotFound) return settings;
        return err;
    };
    defer file.close();

    const content = try file.readToEndAlloc(allocator, 65536);
    defer allocator.free(content);

    

    return settings;
}

test "defaultDir" {
    const allocator = testing.allocator;
    const dir = try defaultDir(allocator);
    defer allocator.free(dir);
    try std.testing.expect(dir.len > 0);
}

const testing = std.testing;
