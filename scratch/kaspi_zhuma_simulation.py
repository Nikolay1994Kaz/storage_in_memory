import socket
import time
import random
import concurrent.futures
import sys
import json

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
    
    if first_char == b"+":
        return read_line(sock).decode('utf-8')
    elif first_char == b"-":
        return "ERROR: " + read_line(sock).decode('utf-8')
    elif first_char == b":":
        return int(read_line(sock))
    elif first_char == b"$":
        length_line = read_line(sock)
        length = int(length_line)
        if length == -1:
            return None
        data = sock.recv(length)
        sock.recv(2) # read trailing \r\n
        return data.decode('utf-8')
    elif first_char == b"*":
        length_line = read_line(sock)
        length = int(length_line)
        if length == -1:
            return None
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

# Один рабочий поток (симулирует сессию клиента банка)
def worker_task(worker_id, num_requests):
    latencies = []
    decisions = {"OK": 0, "REVIEW": 0, "BLOCKED": 0}
    errors = 0
    
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.connect(("127.0.0.1", 6380))
    except Exception as e:
        return latencies, decisions, num_requests # все ошибки
        
    for i in range(num_requests):
        tx_id = f"w{worker_id}_t{i}_{random.randint(100000, 999999)}"
        amount = random.randint(500, 65000)
        
        # 95% транзакций из Казахстана (KZ), 5% из сомнительных стран
        rand_val = random.random()
        if rand_val < 0.95:
            country = "KZ"
        elif rand_val < 0.98:
            country = "NK" # North Korea -> Blocked/High score
        else:
            country = "IR" # Iran -> Blocked/High score
            
        payload = f'{{"amount":{amount},"country":"{country}"}}'
        
        start_time = time.perf_counter()
        
        # 1. Записываем транзакцию в БД -> Автоматически срабатывает WASM триггер
        res_set = execute(sock, ["SET", f"tx:fraud:pending:{tx_id}", payload])
        
        # 2. Получаем результат работы WASM
        res_get = execute(sock, ["GET", f"fraud:tx:fraud:pending:{tx_id}"])
        
        latency = (time.perf_counter() - start_time) * 1000.0 # в миллисекундах
        latencies.append(latency)
        
        if res_get and not res_get.startswith("ERROR:"):
            try:
                data = json.loads(res_get)
                dec = data.get("decision", "UNKNOWN")
                if dec in decisions:
                    decisions[dec] += 1
            except:
                errors += 1
        else:
            errors += 1
            
    sock.close()
    return latencies, decisions, errors

def main():
    print("=== ЗАПУСК СТРЕСС-ТЕСТА: КАСПИ ЖУМА (КАСКО/ИПОТЕКА/РАССРОЧКА) ===")
    print("Цель: Симуляция 5,000 параллельных решений по рассрочке через WASM-триггеры.")
    
    concurrency = 20 # 20 параллельных клиентов
    requests_per_worker = 250 # по 250 транзакций на каждого
    total_tx = concurrency * requests_per_worker
    
    print(f"Запускаем {concurrency} параллельных потоков-клиентов...")
    print(f"Всего транзакций к обработке: {total_tx}")
    
    start_test = time.perf_counter()
    
    all_latencies = []
    total_decisions = {"OK": 0, "REVIEW": 0, "BLOCKED": 0}
    total_errors = 0
    
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = [executor.submit(worker_task, i, requests_per_worker) for i in range(concurrency)]
        for fut in concurrent.futures.as_completed(futures):
            latencies, decisions, errors = fut.result()
            all_latencies.extend(latencies)
            total_errors += errors
            for k, v in decisions.items():
                total_decisions[k] += v
                
    end_test = time.perf_counter()
    duration = end_test - start_test
    
    # Расчет метрик
    all_latencies.sort()
    avg_latency = sum(all_latencies) / len(all_latencies) if all_latencies else 0
    p50 = all_latencies[int(len(all_latencies) * 0.50)] if all_latencies else 0
    p95 = all_latencies[int(len(all_latencies) * 0.95)] if all_latencies else 0
    p99 = all_latencies[int(len(all_latencies) * 0.99)] if all_latencies else 0
    rps = total_tx / duration
    
    print("\n================ РЕЗУЛЬТАТЫ СИМУЛЯЦИИ КАСПИ ЖУМА ================")
    print(f"Длительность теста:       {duration:.2f} сек")
    print(f"Всего обработано заявок:  {total_tx}")
    print(f"Производительность (RPS):  {rps:.0f} решений в секунду (Transactions per Second)")
    print(f"Ошибок / Сбоев системы:   {total_errors} (Идеальный показатель: 0)")
    print(f"\nВремя отклика на одну кредитную заявку (Latency):")
    print(f"  - Среднее время:         {avg_latency:.2f} ms")
    print(f"  - Медиана (p50):         {p50:.2f} ms")
    print(f"  - 95-й перцентиль (p95):  {p95:.2f} ms (95% заявок одобряются быстрее)")
    print(f"  - 99-й перцентиль (p99):  {p99:.2f} ms (худшие случаи)")
    
    print("\nРаспределение принятых решений по рассрочке:")
    print(f"  - [🟢 ОДОБРЕНО (OK)]:          {total_decisions['OK']} ({total_decisions['OK']/total_tx*100:.1f}%)")
    print(f"  - [🟡 РУЧНАЯ ПРОВЕРКА (REVIEW)]: {total_decisions['REVIEW']} ({total_decisions['REVIEW']/total_tx*100:.1f}%)")
    print(f"  - [🔴 ОТКЛОНЕНО (BLOCKED)]:      {total_decisions['BLOCKED']} ({total_decisions['BLOCKED']/total_tx*100:.1f}%)")
    print("=================================================================")

if __name__ == "__main__":
    main()
