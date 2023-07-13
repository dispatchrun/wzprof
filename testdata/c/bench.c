#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>

int64_t now() {
  struct timeval tv;
  gettimeofday(&tv, NULL);
  return ((int64_t)(tv.tv_sec))*1e9 + ((int64_t)(tv.tv_usec))*1e3;
}

int isAbs(const char *path) {
  return *path == '/';
}

int isDir(const char *path) {
  const int n = strlen(path);
  return (n > 0) && path[n-1] == '/';
}

int appendCleanPath(char *buf, int bufLen, const char *path, int *lookupParent) {
  while (*path) {
    while (*path && *path == '/') {
      path++;
    }

    const char *ptr = path;
    const char *end = path;
    while (*end && *end != '/') {
      end++;
    }
    path = end;

    const int len = end - ptr;
    if (len == 0) {
      continue;
    }

    if (len == 1 && ptr[0] == '.') {
      continue;
    }

    if (len == 2 && ptr[0] == '.' && ptr[1] == '.') {
      if (!*lookupParent) {
        int k = bufLen;
        while (k > 0 && buf[k-1] != '/') {
          k--;
        }
        while (k > 1 && buf[k-1] == '/') {
          k--;
        }
        bufLen = k;
        if (k == 0) {
          *lookupParent = 1;
        } else {
          continue;
        }
      }
    } else {
      *lookupParent = 0;
    }

    if (bufLen > 0 && buf[bufLen-1] != '/') {
      buf[bufLen] = '/';
      bufLen++;
    }
    memmove(buf + bufLen, ptr, len);
    bufLen += len;
  }
  return bufLen;
}

char *joinPath(const char *dir, const char *file) {
  int bufSize = strlen(dir) + strlen(file) + 8;
  int bufLen = 0;
  char *buf = malloc(bufSize);
  memset(buf, 0, bufSize);
  if (isAbs(dir)) {
    buf[0] = '/';
    bufLen++;
  }
  int lookupParent = 0;
  bufLen = appendCleanPath(buf, bufLen, dir, &lookupParent);
  bufLen = appendCleanPath(buf, bufLen, file, &lookupParent);
  if (bufLen == 0) {
    buf[0] = '.';
    bufLen++;
  }
  if (buf[bufLen-1] != '/' && isDir(file)) {
    buf[bufLen] = '/';
  }
  return buf;
}

int main(int argc, char **argv) {
  if (argc > 4) {
    puts("usage: join [dir] [file]");
    return 2;
  }

  const char *cmd = "join";
  const char *dir = ".";
  const char *file = ".";
  if (argc > 1) {
    cmd = argv[1];
  }
  if (argc > 2) {
    dir = argv[2];
  }
  if (argc > 3) {
    file = argv[3];
  }

  if (strcmp(cmd, "join") == 0) {
    puts(joinPath(dir, file));
  } else if (strcmp(cmd, "test") == 0) {
    const int n = atoi(argv[2]);
    puts("goos:");
    puts("goarch:");
    puts("pkg:");

    for (int i = 0; i < n; i++) {
      const int64_t start = now();
      const int64_t count = 20e6;

      for (int64_t j = 0; j < count; j++) {
        free(joinPath(dir, file));
      }

      const int64_t end = now();
      printf("BenchmarkJoinPath/#00        %lld\t% 10.2f ns/op\n", count, ((double)(end - start))/count);
      fflush(stdout);
    }

    puts("PASS");
  } else {
    puts("usage: join|test");
    return 2;
  }
  return 0;
}
