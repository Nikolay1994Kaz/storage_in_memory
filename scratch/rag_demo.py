import socket
import time
import sys

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
        # Read trailing \r\n
        sock.recv(2)
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

def main():
    print("=== Запуск симуляции банковского RAG на базе KVStore ===")
    
    # Подключение к порту 6380
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.connect(("127.0.0.1", 6380))
    except Exception as e:
        print(f"Ошибка подключения к KVStore на 127.0.0.1:6380: {e}")
        sys.exit(1)
        
    print("1. Подключение к KVStore успешно установлено.")
    
    # 1. Загрузка документов
    docs = {
        "doc:bank:1": "Банк Alem-AI выдает беззалоговые кредиты на сумму до 5,000,000 тенге для физических лиц. Срок кредитования — от 6 до 60 месяцев.",
        "doc:bank:2": "Для получения кредита в банке Alem-AI заемщик должен предоставить удостоверение личности, выписку о пенсионных отчислениях за последние 6 месяцев и справку о доходах.",
        "doc:bank:3": "В случае просрочки платежа по кредиту в банке Alem-AI начисляется пеня в размере 0.5% от суммы просроченного платежа за каждый день просрочки."
    }
    
    print("\n2. Отправка документов на векторизацию и сохранение (AI.INGEST):")
    for key, text in docs.items():
        print(f" -> Отправка [{key}]: \"{text[:60]}...\"")
        res = execute(sock, ["AI.INGEST", key, text])
        print(f"    Ответ сервера: {res}")
        
    print("\nОжидаем окончания фоновой векторизации в Ollama (3 секунды)...")
    time.sleep(3.5)
    
    # Проверим, что документы лежат в KV хранилище
    print("\n3. Проверка сохраненных данных в KV-хранилище:")
    for key in docs.keys():
        text_in_db = execute(sock, ["GET", key])
        print(f" -> GET {key} = \"{text_in_db[:60]}...\"")
        
    # 2. Поиск по смыслу (Векторный поиск)
    print("\n4. Выполнение семантического поиска (AI.SEARCH) по вопросу о просрочке:")
    query = "Что будет, если я просрочу платеж по кредиту?"
    print(f" -> Вопрос: \"{query}\"")
    search_results = execute(sock, ["AI.SEARCH", "1", query])
    print(f"    Результаты поиска (ближайший вектор): {search_results}")
    
    # 3. Инференс RAG (AI.ASK)
    print("\n5. Запуск полного RAG-цикла (AI.ASK):")
    question = "Каковы условия по сумме и сроку беззалогового кредита?"
    print(f" -> Вопрос: \"{question}\"")
    print(" -> Ожидаем ответ от ИИ...")
    answer = execute(sock, ["AI.ASK", question])
    print(f"\n[Ответ модели Gemma4]: {answer}")
    
    sock.close()
    print("\n=== Симуляция успешно завершена ===")

if __name__ == "__main__":
    main()
