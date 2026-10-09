#!/usr/bin/env python3
"""Drive the no-mistakes TUI through a real pty and press the yolo key.

argv: <no-mistakes binary> <workdir> <keys> <seconds-before-keys> <seconds-after-keys>

The pty is given a non-zero window size before the TUI reads its grid: a 0x0
grid makes bubbletea exit immediately, which would turn this live check into a
fake. The master side is drained continuously and the final screen is written
to stdout.
"""
import fcntl, os, pty, select, signal, struct, sys, termios, time

binary, workdir, keys, pre, post = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4]), float(sys.argv[5])

pid, fd = pty.fork()
if pid == 0:
    os.chdir(workdir)
    os.environ["TERM"] = "xterm-256color"
    os.execv(binary, [binary, "attach"])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 160, 0, 0))

buf = bytearray()

def drain(seconds):
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.2)
        if fd in r:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                return
            if not chunk:
                return
            buf.extend(chunk)

drain(pre)
for key in keys:
    os.write(fd, key.encode())
    drain(0.4)
drain(post)
os.write(fd, b"q")
drain(1.0)
try:
    os.kill(pid, signal.SIGTERM)
except ProcessLookupError:
    pass
os.waitpid(pid, 0)
sys.stdout.write(buf.decode("utf-8", "replace"))
