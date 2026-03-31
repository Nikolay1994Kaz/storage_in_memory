package cluster

var crc16Table [256]uint16

func init() {
	// Строим таблицу при старте программы.
	// Для каждого возможного байта (0–255) предвычисляем CRC.
	for i := 0; i < 256; i++ {
		crc := uint16(i) << 8 // сдвигаем байт в старшие 8 бит
		for j := 0; j < 8; j++ {
			// Если старший бит = 1, XOR с полиномом
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc = crc << 1
			}
		}
		crc16Table[i] = crc
	}
}

func CRC16(data string) uint16 {
	crc := uint16(0)

	for i := 0; i < len(data); i++ {
		b := data[i]

		index := byte(crc>>8) ^ b
		crc = (crc << 8) ^ crc16Table[index]
	}

	return crc

}

const TotalSlots = 16384

func KeySlot(key string) uint16 {
	return CRC16(key) % TotalSlots
}
