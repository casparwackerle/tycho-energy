# import os, time, torch

# torch.set_num_threads(1)

# # Knobs
# secs = 60
# dtype = torch.float32   # try float32 if you want lower peak throughput
# n = 3072                # matrix size; tune this (4096..16384)
# streams = 1             # increase to raise load smoothly (1..8)
# sleep_ms = 0.0          # tiny sleep can fine-tune downward (0..2ms)

# dev = "cuda:0"
# torch.backends.cuda.matmul.allow_tf32 = True
# a = torch.randn((n, n), device=dev, dtype=dtype)
# b = torch.randn((n, n), device=dev, dtype=dtype)

# ss = [torch.cuda.Stream() for _ in range(streams)]
# end = time.time() + secs

# # Warmup
# for _ in range(10):
#     c = a @ b
# torch.cuda.synchronize()

# i = 0
# while time.time() < end:
#     s = ss[i % streams]
#     with torch.cuda.stream(s):
#         c = a @ b
#     i += 1
#     if sleep_ms:
#         time.sleep(sleep_ms / 1000.0)

# torch.cuda.synchronize()
# print("done")

import time, torch
torch.set_num_threads(1)

gpu = 0
secs = 60
load = 0.10          # target fraction (0.0..1.0)
period_ms = 10       # control period (10..100ms works well)
dtype = torch.float16
n = 3072             # start smaller

torch.cuda.set_device(gpu)
a = torch.randn((n, n), device="cuda", dtype=dtype)
b = torch.randn((n, n), device="cuda", dtype=dtype)

# Warmup
for _ in range(5):
    a @ b
torch.cuda.synchronize()

end = time.time() + secs
period = period_ms / 1000.0
busy = period * load

while time.time() < end:
    t0 = time.time()
    # Busy phase: keep launching GEMMs until busy time elapsed
    while (time.time() - t0) < busy:
        a @ b
        sleep_ms = 0.05
    torch.cuda.synchronize()
    # Idle remainder of period
    dt = time.time() - t0
    if dt < period:
        time.sleep(period - dt)

print("done")
