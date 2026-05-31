// Package paths provides common file paths used by both gmux and gmuxd.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// StateDir returns the gmux state directory (~/.local/state/gmux).
pub fn stateDir(allocator: Allocator) ![]const u8 {
    if (std.process.getEnvVarOwned(allocator, "XDG_STATE_HOME")) |dir| {
        return std.fs.path.join(allocator, &.{ dir, "gmux" });
    }

    const home = try homeDir(allocator);
    defer allocator.free(home);
    return std.fs.path.join(allocator, &.{ home, ".local", "state", "gmux" });
}

/// SocketPath returns the path to the gmuxd Unix socket for local IPC.
pub fn socketPath(allocator: Allocator) ![]const u8 {
    const state = try stateDir(allocator);
    defer allocator.free(state);
    return std.fs.path.join(allocator, &.{ state, "gmuxd.sock" });
}

/// SessionsDir returns the directory under StateDir that holds
/// per-session subdirectories.
pub fn sessionsDir(allocator: Allocator) ![]const u8 {
    const state = try stateDir(allocator);
    defer allocator.free(state);
    return std.fs.path.join(allocator, &.{ state, "sessions" });
}

/// SessionDir returns the per-session subdirectory for id under SessionsDir.
pub fn sessionDir(allocator: Allocator, id: []const u8) ![]const u8 {
    const sessions = try sessionsDir(allocator);
    defer allocator.free(sessions);
    return std.fs.path.join(allocator, &.{ sessions, id });
}

/// NormalizePath expands a stored path to its absolute form for use in
/// filesystem operations. Expands ~ prefix to $HOME and calls path cleanup.
pub fn normalizePath(allocator: Allocator, p: []const u8) ![]const u8 {
    if (p.len == 0) {
        return allocator.dupe(u8, p);
    }

    if (std.mem.eql(u8, p, "~")) {
        const home = try homeDir(allocator);
        errdefer allocator.free(home);
        const resolved = try std.fs.path.resolve(allocator, &.{home});
        allocator.free(home);
        return resolved;
    }

    if (std.mem.startsWith(u8, p, "~/")) {
        const home = try homeDir(allocator);
        errdefer allocator.free(home);
        const rest = p[2..];
        const joined = try std.fs.path.join(allocator, &.{ home, rest });
        allocator.free(home);
        errdefer allocator.free(joined);
        return std.fs.path.resolve(allocator, &.{joined});
    }

    return std.fs.path.resolve(allocator, &.{p});
}

/// CanonicalizePath converts an absolute filesystem path to its canonical
/// stored form: symlinks resolved, $HOME prefix replaced with ~.
pub fn canonicalizePath(allocator: Allocator, abs: []const u8) ![]const u8 {
    if (abs.len == 0) {
        return allocator.dupe(u8, abs);
    }

    // Resolve symlinks (best effort)
    const resolved = try std.fs.resolveSymbolicLinks(allocator, abs);
    errdefer allocator.free(resolved);
    const cleaned = try std.fs.path.resolve(allocator, &.{resolved});
    allocator.free(resolved);
    errdefer allocator.free(cleaned);

    const home = try homeDir(allocator);
    defer allocator.free(home);
    const homeCleaned = try std.fs.path.resolve(allocator, &.{home});
    defer allocator.free(homeCleaned);

    if (std.mem.eql(u8, cleaned, homeCleaned)) {
        allocator.free(cleaned);
        return allocator.dupe(u8, "~");
    }

    // Check if path is under home
    if (std.mem.startsWith(u8, cleaned, homeCleaned)) {
        const rest = cleaned[homeCleaned.len..];
        allocator.free(cleaned);
        return std.mem.prepend(allocator, u8, "~", rest);
    }

    return cleaned;
}

fn homeDir(allocator: Allocator) ![]const u8 {
    if (std.process.getEnvVarOwned(allocator, "HOME")) |home| {
        return home;
    }
    return error.HomeDirNotFound;
}

test "stateDir with XDG_STATE_HOME" {
    const allocator = testing.allocator;
    std.process.setEnvVar("XDG_STATE_HOME", "/custom/state", true);
    defer std.process.setEnvVar("XDG_STATE_HOME", null, true);

    const result = try stateDir(allocator);
    defer allocator.free(result);
    try std.testing.expectEqualStrings("/custom/state/gmux", result);
}

test "normalizePath tilde" {
    const allocator = testing.allocator;
    const result = try normalizePath(allocator, "~");
    defer allocator.free(result);
    const home = try std.process.getEnvVar(allocator, "HOME");
    try std.testing.expect(std.mem.startsWith(u8, result, home));
}

test "normalizePath tilde-slash" {
    const allocator = testing.allocator;
    const result = try normalizePath(allocator, "~/projects");
    defer allocator.free(result);
    const home = try std.process.getEnvVar(allocator, "HOME");
    try std.testing.expect(std.mem.startsWith(u8, result, home));
    try std.testing.expect(std.mem.endsWith(u8, result, "projects"));
}

test "normalizePath empty" {
    const allocator = testing.allocator;
    const result = try normalizePath(allocator, "");
    defer allocator.free(result);
    try std.testing.expectEqualStrings("", result);
}

test "canonicalizePath home" {
    const allocator = testing.allocator;
    const home = try std.process.getEnvVarOwned(allocator, "HOME");
    defer allocator.free(home);

    const result = try canonicalizePath(allocator, home);
    defer allocator.free(result);
    try std.testing.expectEqualStrings("~", result);
}

const testing = std.testing;
