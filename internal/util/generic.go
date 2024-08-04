package util

func MergeMaps[K comparable, V any](maps ...map[K]V) map[K]V {
	baseMap := make(map[K]V, len(maps)*10) // preallocate 10 slots per map
	for _, m := range maps {
		for k, v := range m {
			baseMap[k] = v
		}
	}
	return baseMap
}
