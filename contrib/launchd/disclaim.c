// disclaim: run a program as its own TCC-responsible process, the way a
// terminal does for its shells. grant.sh uses it to make one access through
// the installed launcher, so the grant is checked against the launcher rather
// than against the terminal it was started from.

#include <spawn.h>
#include <stdio.h>
#include <string.h>
#include <sys/wait.h>

extern char **environ;
extern int responsibility_spawnattrs_setdisclaim(posix_spawnattr_t *attrs, int disclaim);

int main(int argc, char *argv[]) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s program [args...]\n", argv[0]);
        return 64;
    }

    posix_spawnattr_t attrs;
    posix_spawnattr_init(&attrs);
    responsibility_spawnattrs_setdisclaim(&attrs, 1);

    pid_t pid;
    int err = posix_spawn(&pid, argv[1], NULL, &attrs, &argv[1], environ);
    if (err != 0) {
        fprintf(stderr, "%s: %s: %s\n", argv[0], argv[1], strerror(err));
        return 127;
    }

    int status;
    waitpid(pid, &status, 0);
    return WIFEXITED(status) ? WEXITSTATUS(status) : 128 + WTERMSIG(status);
}
