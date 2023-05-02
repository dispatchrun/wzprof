fn main() {
    let a = allocate_memory(10);
    let b = allocate_memory(20);
    let c = allocate_memory(30);
    println!("Allocated memory: a={:?}, b={:?}, c={:?}", a, b, c);
}

fn allocate_memory(size: usize) -> Vec<i32> {
    let mut vec = Vec::with_capacity(size);
    for i in 0..size {
        vec.push(i as i32);
    }
    let d = allocate_more_memory(size);
    vec.extend(d);
    vec
}

fn allocate_more_memory(size: usize) -> Vec<i32> {
    let mut vec = Vec::with_capacity(size);
    for i in 0..size {
        vec.push((i as i32) * 2);
    }
    let e = allocate_even_more_memory(size);
    vec.extend(e);
    vec
}

fn allocate_even_more_memory(size: usize) -> Vec<i32> {
    let mut vec = Vec::with_capacity(size);
    for i in 0..size {
        vec.push((i as i32) * 3);
    }
    vec
}