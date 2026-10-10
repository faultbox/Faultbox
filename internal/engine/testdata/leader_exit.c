// A thread-group leader may be a zombie while a worker still owns a socket.
// argv: ready file, release file. No daemon or unrelated host resource is used.
#include <arpa/inet.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
static char **args;
static void *worker(void *unused) {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in addr = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    const char *port = getenv("FB_TEST_PORT");
    if (port) addr.sin_port = htons(atoi(port));
    if (fd < 0 || bind(fd, (void *)&addr, sizeof(addr)) || listen(fd, 4)) _exit(80);
    socklen_t size = sizeof(addr);
    if (getsockname(fd, (void *)&addr, &size)) _exit(81);
    FILE *ready = fopen(args[1], "w");
    if (!ready) _exit(82);
    fprintf(ready, "%d", ntohs(addr.sin_port)); fclose(ready);
    while (access(args[2], F_OK)) usleep(5000);
    close(fd);
    _exit(23);
    return NULL;
}
int main(int argc, char **argv) {
    if (argc != 3 && argc != 4) return 83;
    args = argv;
    if (argc == 4) {
        pid_t child = fork();
        if (child < 0) return 85;
        if (child == 0) { setsid(); worker(NULL); }
        while (access(args[2], F_OK)) usleep(5000);
        _exit(23);
    }
    pthread_t thread;
    if (pthread_create(&thread, NULL, worker, NULL)) return 84;
    pthread_exit(NULL);
}
