import socket
import time
import random
import concurrent.futures
import sys
import json
import threading

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

# Глобальный флаг для остановки воркеров по времени
stop_test = False
global_stats = {
    "total_tx": 0,
    "ok": 0,
    "review": 0,
    "blocked": 0,
    "errors": 0,
    "latencies": []
}
stats_lock = threading.Lock()

def worker_task(worker_id):
    global stop_test
    
    local_latencies = []
    local_decisions = {"OK": 0, "REVIEW": 0, "BLOCKED": 0}
    local_errors = 0
    local_tx = 0
    
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.connect(("127.0.0.1", 6380))
    except Exception as e:
        print(f"Worker {worker_id} failed to connect: {e}")
        return
        
    while not stop_test:
        tx_id = f"w{worker_id}_t{local_tx}_{random.randint(100000, 999999)}"
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
        
        try:
            # 1. Записываем транзакцию в БД -> Автоматически срабатывает WASM триггер
            res_set = execute(sock, ["SET", f"tx:fraud:pending:{tx_id}", payload])
            
            # 2. Получаем результат работы WASM
            res_get = execute(sock, ["GET", f"fraud:tx:fraud:pending:{tx_id}"])
            
            latency = (time.perf_counter() - start_time) * 1000.0 # в миллисекундах
            local_latencies.append(latency)
            local_tx += 1
            
            if res_get and not res_get.startswith("ERROR:"):
                data = json.loads(res_get)
                dec = data.get("decision", "UNKNOWN")
                if dec in local_decisions:
                    local_decisions[dec] += 1
            else:
                local_errors += 1
                
            # 3. Чистим за собой ключи, чтобы in-memory база не лопнула за 5 минут теста
            execute(sock, ["DEL", f"tx:fraud:pending:{tx_id}"])
            execute(sock, ["DEL", f"fraud:tx:fraud:pending:{tx_id}"])
            
        except Exception as e:
            local_errors += 1
            # В случае разрыва соединения пробуем переподключиться
            try:
                sock.close()
                sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                sock.connect(("127.0.0.1", 6380))
            except:
                pass
            
    sock.close()
    
    # Сохраняем статистику
    with stats_lock:
        global_stats["total_tx"] += local_tx
        global_stats["errors"] += local_errors
        global_stats["ok"] += local_decisions["OK"]
        global_stats["review"] += local_decisions["REVIEW"]
        global_stats["blocked"] += local_decisions["BLOCKED"]
        global_stats["latencies"].extend(local_latencies)

def monitor_progress(duration_seconds):
    global stop_test
    start_time = time.time()
    last_tx = 0
    last_check_time = start_time
    
    print("Сессия нагрузочного тестирования началась...")
    
    while time.time() - start_time < duration_seconds:
        time.sleep(5)
        current_time = time.time()
        elapsed = current_time - start_time
        
        with stats_lock:
            current_tx = global_stats["total_tx"]
            current_errors = global_stats["errors"]
            
        interval_tx = current_tx - last_tx
        interval_time = current_time - last_check_time
        interval_rps = interval_tx / interval_time if interval_time > 0 else 0
        
        print(f"[{elapsed:.0f}s / {duration_seconds}s] Обработано транзакций: {current_tx} (Текущий RPS: {interval_rps:.0f}), Ошибок: {current_errors}")
        
        last_tx = current_tx
        last_check_time = current_time
        
    stop_test = True
    print("Время теста истекло, останавливаем генераторы нагрузки...")

def main():
    duration = 300 # 5 минут = 300 секунд
    concurrency = 20 # 20 параллельных потоков-клиентов
    
    print("=== ДЛИТЕЛЬНЫЙ НАГРУЗОЧНЫЙ ТЕСТ: КАСПИ ЖУМА (5 МИНУТ) ===")
    print(f"Параллельных клиентов: {concurrency}")
    print(f"Планируемая длительность: {duration} секунд")
    print("База автоматически очищается от временных ключей транзакций во время работы.")
    print("=========================================================================")
    
    # Запускаем поток мониторинга прогресса
    monitor_thread = threading.Thread(target=monitor_progress, args=(duration,))
    monitor_thread.start()
    
    start_time_perf = time.perf_counter()
    
    # Запускаем воркеры в ThreadPool
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        futures = [executor.submit(worker_task, i) for i in range(concurrency)]
        concurrent.futures.wait(futures)
        
    monitor_thread.join()
    
    end_time_perf = time.perf_counter()
    total_duration = end_time_perf - start_time_perf
    
    latencies = global_stats["latencies"]
    latencies.sort()
    
    avg_latency = sum(latencies) / len(latencies) if latencies else 0
    p50 = latencies[int(len(latencies) * 0.50)] if latencies else 0
    p95 = latencies[int(len(latencies) * 0.95)] if latencies else 0
    p99 = latencies[int(len(latencies) * 0.99)] if latencies else 0
    total_tx = global_stats["total_tx"]
    rps = total_tx / total_duration if total_duration > 0 else 0
    
    print("\n================ ОКОНЧАТЕЛЬНЫЕ РЕЗУЛЬТАТЫ (5 МИНУТ) ================")
    print(f"Фактическая длительность:   {total_duration:.2f} сек")
    print(f"Всего успешно транзакций:   {total_tx}")
    print(f"Средняя производительность:  {rps:.0f} решений/сек (RPS)")
    print(f"Всего ошибок / сбоев:       {global_stats['errors']}")
    print(f"\nВремя отклика на транзакцию (Latency):")
    print(f"  - Среднее время:          {avg_latency:.2f} ms")
    print(f"  - Медиана (p50):          {p50:.2f} ms")
    print(f"  - 95-й перцентиль (p95):   {p95:.2f} ms")
    print(f"  - 99-й перцентиль (p99):   {p99:.2f} ms")
    
    print("\nРаспределение решений:")
    print(f"  - [🟢 ОДОБРЕНО (OK)]:          {global_stats['ok']} ({global_stats['ok']/total_tx*100:.1f}%)" if total_tx > 0 else "")
    print(f"  - [🟡 РУЧНАЯ ПРОВЕРКА (REVIEW)]: {global_stats['review']} ({global_stats['review']/total_tx*100:.1f}%)" if total_tx > 0 else "")
    print(f"  - [🔴 ОТКЛОНЕНО (BLOCKED)]:      {global_stats['blocked']} ({global_stats['blocked']/total_tx*100:.1f}%)" if total_tx > 0 else "")
    print("====================================================================")

if __name__ == "__main__":
    main()
