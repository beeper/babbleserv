tests = []
with open("docker/complement-tests.list", "r") as f:
    lines = f.read().splitlines()

for line in lines:
    line = line.strip()
    if line.startswith("#") or line == "":
        continue
    tests.append(line)

print("|".join(tests))
