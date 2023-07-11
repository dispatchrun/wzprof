def a():
    print("world")
    d = bytearray(100)
    print(len(d))

def b():
    a()

def c():
    print("hello")
    b()
    print("!")

if __name__ == "__main__":
    c()