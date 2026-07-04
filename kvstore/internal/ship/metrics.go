package ship

import "kvstore/kvstore/internal/monitoring"

// Тонкая прослойка к monitoring: пакет ship не знает про VictoriaMetrics,
// monitoring не знает про ship (та же дисциплина, что у wal/syncer).

func shipErrorsInc()       { monitoring.ShipErrors.Inc() }
func shipBytesAdd(n int64) { monitoring.ShipBytes.Add(int(n)) }
