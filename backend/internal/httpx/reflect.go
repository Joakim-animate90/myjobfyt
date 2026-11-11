package httpx

import "reflect"

// reflectStructField aliases reflect.StructField so the validator tag-name hook reads
// clearly at its single call site in binding.go.
type reflectStructField = reflect.StructField
