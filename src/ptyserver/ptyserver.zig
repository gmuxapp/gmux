// Package ptyserver allocates a PTY, execs a command, and serves WebSocket.
const std = @import("std");
const Allocator = std.mem.Allocator;
const session = @import("../session/state.zig");
const adapter = @import("../adapter/adapter.zig");

/// Server holds a PTY and serves WebSocket connections.
pub const Server = struct {
    allocator: Allocator,
    cmd: *ChildProcess,
    ptmx: std.posix_fd,
    sockPath: []const u8,
    state: *session.State,
    adapter: ?*adapter.AdapterBase,
    clients: std.ArrayList(*WSClient),
    localOut: ?*PTYWriter,
    scrollback: ?*ScrollbackWriter,
    ptyCols: u16,
    ptyRows: u16,
    done: bool,
    err: ?anyerror,

    const posix_fd = c_int;

    pub fn init(allocator: Allocator, cfg: Config) !*Server {
        const self = try allocator.create(Server);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sockPath = try allocator.dupe(u8, cfg.socketPath);
        self.state = cfg.state;
        self.adapter = cfg.adapter;
        self.clients = std.ArrayList(*WSClient).init(allocator);
        self.localOut = cfg.localOut;
        self.scrollback = cfg.scrollback;
        self.ptyCols = cfg.cols;
        self.ptyRows = cfg.rows;
        self.done = false;
        self.err = null;

        // Allocate PTY
        const pty = try allocatePTY();
        self.ptmx = pty.master;

        // Fork and exec
        self.cmd = try forkAndExec(allocator, cfg.command, cfg.cwd, cfg.env, pty.slave);

        return self;
    }

    pub fn deinit(self: *Server) void {
        self.allocator.free(self.sockPath);
        self.clients.deinit();
        std.posix.close(self.ptmx);
        self.allocator.destroy(self);
    }

    /// allocatePTY allocates a new PTY.
    fn allocatePTY() !struct { master: posix_fd, slave: posix_fd } {
        var slave_name: [64]u8 = undefined;
        const master = std.posix.openpty(&slave_name) catch return error.PTYAllocFailed;
        const slave = std.posix.open(slave_name, .{ .access = .rdwr, .flags = .nonblock }, 0);
        return .{ .master = master, .slave = slave };
    }

    /// forkAndExec forks a child process and execs the command.
    fn forkAndExec(allocator: Allocator, command: []const []const u8, cwd: []const u8, env: []const []const u8, slave: posix_fd) !*ChildProcess {
        const pid = std.posix.fork();
        if (pid == 0) {
            // Child
            std.posix.close(0);
            std.posix.dup2(slave, 0);
            std.posix.dup2(slave, 1);
            std.posix.dup2(slave, 2);
            if (slave > 2) std.posix.close(slave);

            // Set cwd
            std.posix.fchdir(cwd);

            // Exec
            var args = std.ArrayList([]const u8).init(allocator);
            for (command) |c| args.append(c) catch {};
            args.append("") catch {};
            std.posix.execve(command[0], args.items, env);
            std.posix._exit(1);
        }

        return allocator.create(ChildProcess) catch unreachable;
    }
};

/// Config for creating a new PTY server.
pub const Config = struct {
    command: []const []const u8,
    cwd: []const u8,
    env: []const []const u8,
    socketPath: []const u8,
    cols: u16,
    rows: u16,
    state: *session.State,
    adapter: ?*adapter.AdapterBase,
    localOut: ?*PTYWriter,
    scrollback: ?*ScrollbackWriter,
};

/// ChildProcess represents a child process.
pub const ChildProcess = struct {
    pid: std.posix.pid_t,

    pub fn wait(self: *ChildProcess) !c_int {
        var status: c_int = undefined;
        _ = std.posix.waitpid(self.pid, &status, 0);
        if (std.posix.WIFEXITED(status)) {
            return std.posix.WEXITSTATUS(status);
        }
        return 1;
    }
};

/// WSClient represents a WebSocket client.
pub const WSClient = struct {
    conn: *WebSocketConn,
};

/// WebSocketConn represents a WebSocket connection.
pub const WebSocketConn = struct {
    // In real implementation, this would hold the WebSocket connection
};

/// PTYWriter is a writer for PTY data.
pub const PTYWriter = struct {
    fd: std.posix_fd,

    pub fn write(self: *PTYWriter, data: []const u8) void {
        std.posix.writeAll(self.fd, data) catch {};
    }
};

/// ScrollbackWriter writes to scrollback.
pub const ScrollbackWriter = struct {
    // In real implementation, this would write to scrollback
};
