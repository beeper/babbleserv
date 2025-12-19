package util

import "fmt"

func Memoize[T any](f func() (*T, error)) func() (*T, error) {
	var i *T = nil
	return func() (_ *T, err error) {
		if i != nil {
			return i, nil
		}
		i, err = f()
		return i, err
	}
}

func MemoizeMap[K comparable, T any](f func(k K) (*T, error), initialSize int) func(k K) (*T, error) {
	cache := make(map[K]*T, initialSize)
	return func(k K) (*T, error) {
		if i, ok := cache[k]; ok {
			return i, nil
		}
		i, err := f(k)
		if err != nil {
			return nil, err
		}
		cache[k] = i
		return i, nil
	}
}

func StringersToStrs[T fmt.Stringer](ids []T) []string {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	return strs
}
