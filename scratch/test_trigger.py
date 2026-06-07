import socket

def resp_encode(args):
    cmd = f"*{len(args)}\r\n"
    for arg in args:
        arg_str = str(arg)
        cmd += f"${len(arg_str.encode('utf-8'))}\r\n{arg_str}\r\n"
    return cmd.encode('utf-8')

def read_line(sock):
    line = b""
    while True:
        char = sock.recv(1)
        if not char:
            break
        line += char
        if line.endswith(b"\r\n"):
            break
    return line[:-2]

def read_resp(sock):
    first_char = sock.recv(1)
    if not first_char:
        return None
    
    print(f"DEBUG: first char = {first_char}")
    if first_char == b"+":
        return read_line(sock).decode('utf-8')
    elif first_char == b"-":
        return "ERROR: " + read_line(sock).decode('utf-8')
    elif first_char == b":":
        return int(read_line(sock))
    elif first_char == b"$":
        length_line = read_line(sock)
        length = int(length_line)
        print(f"DEBUG: bulk length = {length}")
        if length == -1:
            return None
        data = sock.recv(length)
        sock.recv(2) # read trailing \r\n
        return data.decode('utf-8')
    elif first_char == b"*":
        length_line = read_line(sock)
        length = int(length_line)
        arr = []
        for _ in range(length):
            arr.append(read_resp(sock))
        return arr
    else:
        raise ValueError(f"Unknown RESP type: {first_char}")

def execute(sock, args):
    payload = resp_encode(args)
    sock.sendall(payload)
    return read_resp(sock)

sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.connect(("127.0.0.1", 6380))

print("SETTING KEY...")
res1 = execute(sock, ["SET", "tx:fraud:pending:test", '{"amount":25000,"country":"KZ"}'])
print(f"SET Result: {res1}")

print("\nGETTING ORIGINAL KEY...")
res2 = execute(sock, ["GET", "tx:fraud:pending:test"])
print(f"GET Original Result: {res2}")

print("\nGETTING RESULT KEY...")
res3 = execute(sock, ["GET", "fraud:tx:fraud:pending:test"])
print(f"GET Result Key Result: {res3}")

sock.close()
