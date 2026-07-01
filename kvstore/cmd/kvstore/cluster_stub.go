//go:build !experimental

package main

import (
	"fmt"

	"kvstore/kvstore/internal/store"
	"kvstore/kvstore/internal/store/tcmalloc"
	"kvstore/kvstore/vector"
)

// newClusterRouter в обычной (прод) сборке — заглушка: кластерный код не линкуется,
// а флаг --cluster даёт внятную ошибку вместо тихого включения полу-готового HA.
// Реальная реализация — в cluster_experimental.go (тег `experimental`).
func newClusterRouter(addr string, gossipPort, slotStart, slotEnd int,
	s *tcmalloc.TCMallocStore, vecStore vector.VectorIndex, ttl *store.TTLManager) (clusterNode, error) {
	return nil, fmt.Errorf("кластерный режим доступен только в experimental-сборке " +
		"(go build -tags experimental); в прод-сборке он намеренно отключён")
}
