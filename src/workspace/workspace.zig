// Package workspace detects VCS workspace roots for jj and git repositories.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// DetectRoot walks up from dir looking for jj or git repository markers.
/// Returns the workspace root, or null if no VCS root is found.
pub fn detectRoot(allocator: Allocator, dir: []const u8) !?[]const u8 {
    const abs = try std.fs.path.resolve(allocator, &.{dir});
    errdefer allocator.free(abs);

    var cur = abs;
    while (true) {
        // Check jj first (preferred when colocated with git)
        if (checkJJ(allocator, cur)) |root| {
            allocator.free(cur);
            return root;
        }

        if (checkGit(allocator, cur)) |root| {
            allocator.free(cur);
            return root;
        }

        const parent = std.fs.path.dirname(cur);
        if (parent == null or std.mem.eql(u8, cur, parent.?)) {
            // Reached filesystem root
            allocator.free(cur);
            return null;
        }

        const new_cur = try allocator.dupe(allocator, parent.?);
        allocator.free(cur);
        cur = new_cur;
    }
}

/// checkJJ checks for a .jj directory at dir and resolves the workspace root.
fn checkJJ(allocator: Allocator, dir: []const u8) ?[]const u8 {
    const jjDir = try std.fs.path.join(allocator, &.{ dir, ".jj" });
    defer allocator.free(jjDir);

    const stat = std.fs.statFileAbsolute(jjDir) catch null;
    if (stat == null or !stat.?.isDirectory) {
        return null;
    }

    const repoPath = try std.fs.path.join(allocator, &.{ jjDir, "repo" });
    defer allocator.free(repoPath);

    const repoStat = std.fs.statFileAbsolute(repoPath) catch null;
    if (repoStat) |repoInfo| {
        if (repoInfo.isDirectory) {
            // Main workspace: .jj/repo is the store directory
            return allocator.dupe(allocator, dir);
        }

        // Secondary workspace: .jj/repo is a file with a path
        const data = std.fs.readFileAlloc(allocator, repoPath) catch null;
        if (data) |content| {
            defer allocator.free(content);
            var target = std.mem.trim(u8, content, " \t\r\n");
            if (target.len == 0) {
                return allocator.dupe(allocator, dir);
            }

            // If not absolute, resolve relative to .jj directory
            if (!std.fs.path.isAbsolute(target)) {
                const joined = std.fs.path.join(allocator, &.{ jjDir, target }) catch {
                    return allocator.dupe(allocator, dir);
                };
                // target is now a slice of the newly allocated 'joined'
                // We need to take ownership of joined
                target = joined;
            } else {
                // target is still a slice of content; make an owned copy
                target = allocator.dupe(allocator, target) catch {
                    return allocator.dupe(allocator, dir);
                };
            }
            defer allocator.free(target);

            // target is like /path/to/main-workspace/.jj/repo
            // Main workspace root is two levels up
            const mainJJ = std.fs.path.dirname(target) orelse return allocator.dupe(allocator, dir);
            const mainRoot = std.fs.path.dirname(mainJJ) orelse return allocator.dupe(allocator, dir);
            return allocator.dupe(allocator, mainRoot);
        }
    }

    return allocator.dupe(allocator, dir);
}

/// checkGit checks for a .git entry at dir and resolves the workspace root.
fn checkGit(allocator: Allocator, dir: []const u8) ?[]const u8 {
    const gitPath = try std.fs.path.join(allocator, &.{ dir, ".git" });
    defer allocator.free(gitPath);

    const stat = std.fs.statFileAbsolute(gitPath) catch null;
    if (stat == null) {
        return null;
    }

    if (stat.?.isDirectory) {
        // Regular git repo
        return allocator.dupe(allocator, dir);
    }

    // .git is a file (worktree marker)
    const data = std.fs.readFileAlloc(allocator, gitPath) catch null;
    if (data) |content| {
        defer allocator.free(content);
        var line = std.mem.trim(u8, content, " \t\r\n");

        if (std.mem.startsWith(u8, line, "gitdir: ")) {
            var gitdir = line["gitdir: ".len..];
            if (!std.fs.path.isAbsolute(gitdir)) {
                const joined = std.fs.path.join(allocator, &.{ dir, gitdir }) catch {
                    return allocator.dupe(allocator, dir);
                };
                defer allocator.free(joined);
                gitdir = joined;
            } else {
                // Make owned copy of the slice
                gitdir = allocator.dupe(allocator, gitdir) catch {
                    return allocator.dupe(allocator, dir);
                };
                defer allocator.free(gitdir);
            }

            const mainGitDir = resolveMainGitDir(allocator, gitdir);
            if (mainGitDir) |mgd| {
                defer allocator.free(mgd);
                const root = std.fs.path.dirname(mgd) orelse return allocator.dupe(allocator, dir);
                return allocator.dupe(allocator, root);
            }
        }
    }

    return allocator.dupe(allocator, dir);
}

/// resolveMainGitDir walks up from a worktree gitdir path to find the main .git directory.
fn resolveMainGitDir(allocator: Allocator, gitdir: []const u8) ?[]const u8 {
    // Check commondir file first
    const commondir = try std.fs.path.join(allocator, &.{ gitdir, "commondir" });
    defer allocator.free(commondir);

    const data = std.fs.readFileAlloc(allocator, commondir) catch null;
    if (data) |content| {
        defer allocator.free(content);
        var target = std.mem.trim(u8, content, " \t\r\n");
        if (!std.fs.path.isAbsolute(target)) {
            const joined = std.fs.path.join(allocator, &.{ gitdir, target }) catch null;
            if (joined) |t| {
                target = t;
            }
        }
        return allocator.dupe(allocator, target);
    }

    // Fallback: standard .git/worktrees/<name> layout
    const parent = std.fs.path.dirname(gitdir) orelse return null;
    const base = std.fs.path.basename(parent);
    if (std.mem.eql(u8, base, "worktrees")) {
        return std.fs.path.dirname(parent) orelse null;
    }

    return null;
}

test "detectRoot finds jj directory" {
    const allocator = testing.allocator;
    const tmpDir = "/tmp/test-jj-repo-zig";

    // Create test directory structure
    std.fs.deleteTreeAbsolute(tmpDir) catch {};
    try std.fs.makeDirAbsolute(tmpDir);
    defer std.fs.deleteTreeAbsolute(tmpDir) catch {};

    try std.fs.makeDirAbsolute(tmpDir ++ "/.jj");
    try std.fs.makeDirAbsolute(tmpDir ++ "/.jj/repo");

    const result = try detectRoot(allocator, tmpDir);
    defer if (result) |r| allocator.free(r);

    try std.testing.expect(result != null);
    try std.testing.expect(std.mem.eql(u8, result.?, tmpDir));
}

test "detectRoot finds git directory" {
    const allocator = testing.allocator;
    const tmpDir = "/tmp/test-git-repo-zig";

    std.fs.deleteTreeAbsolute(tmpDir) catch {};
    try std.fs.makeDirAbsolute(tmpDir);
    defer std.fs.deleteTreeAbsolute(tmpDir) catch {};

    try std.fs.makeDirAbsolute(tmpDir ++ "/.git");

    const result = try detectRoot(allocator, tmpDir);
    defer if (result) |r| allocator.free(r);

    try std.testing.expect(result != null);
    try std.testing.expect(std.mem.eql(u8, result.?, tmpDir));
}

test "detectRoot no vcs" {
    const allocator = testing.allocator;
    const tmpDir = "/tmp/test-no-vcs-zig";

    std.fs.deleteTreeAbsolute(tmpDir) catch {};
    try std.fs.makeDirAbsolute(tmpDir);
    defer std.fs.deleteTreeAbsolute(tmpDir) catch {};

    const result = try detectRoot(allocator, tmpDir);
    defer if (result) |r| allocator.free(r);

    try std.testing.expect(result == null);
}

const testing = std.testing;
