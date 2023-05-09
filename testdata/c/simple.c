#include <stdio.h>
#include <stdlib.h>

__attribute__((noinline))
void func1() {
  void* p = malloc(10);
  printf("func1 malloc(10): %p\n", p);
}

__attribute__((noinline))
void func21() {
  void* p = malloc(20);
  printf("func21 malloc(20): %p\n", p);
}

__attribute__((noinline))
void func2() {
  func21();
}

 __attribute__((always_inline))
void func31() {
  void* p = malloc(30);
  printf("func31 malloc(30): %p\n", p);
}

__attribute__((noinline))
void func3() {
  func31();
}

int main(int argc, char** argv) {
  printf("start\n");
  func1();
  func2();
  func3();
  printf("end\n");
}
