// roost-launcher: run a program as a child of this binary.
//
// launchd makes a job's own executable the process macOS privacy checks (TCC)
// hold responsible for everything the job does. Writing to a cloud storage
// folder under ~/Library/CloudStorage needs Full Disk Access, and granting it
// to roost itself would be lost on every upgrade, since TCC keys a grant on
// the binary's code hash. With this small, rarely changing binary in front,
// it alone holds the grant. It must spawn, not exec: exec would make the
// child the responsible process again.

#include <errno.h>
#include <spawn.h>
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>

extern char **environ;

int main(int argc, char *argv[]) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s program [args...]\n", argv[0]);
        return 64;
    }

    pid_t pid;
    int err = posix_spawn(&pid, argv[1], NULL, NULL, &argv[1], environ);
    if (err != 0) {
        fprintf(stderr, "%s: %s: %s\n", argv[0], argv[1], strerror(err));
        return 127;
    }

    int status;
    while (waitpid(pid, &status, 0) < 0) {
        if (errno != EINTR) {
            perror("waitpid");
            return 1;
        }
    }
    return WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
}
