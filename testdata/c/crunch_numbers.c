#include <stdio.h>

int isPrime(int n) {
  if (n == 2 || n == 3) {
    return 1;
  }

  if (n <= 1 || (n%2) == 0 || (n%3) == 0) {
    return 0;
  }

  for (int i = 5; (i * i) <= n; i += 6) {
    if ((n%i) == 0 || (n%(i+2)) == 0) {
      return 0;
    }
  }

  return 1;
}

int main() {
  int rc = 0;
  for (int i = 0; 1; i++) {
    if (isPrime(i)) {
      rc = i;
      // printf("%d\n", i);
    }
  }
  return rc;
}
