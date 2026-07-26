package peering

// projectionSink converts the sink argument into a ProjectionSink.
// Accepts nil or a ProjectionSink; panics on unsupported types.
func projectionSink(v any) ProjectionSink {
	switch x := v.(type) {
	case nil:
		return nil
	case ProjectionSink:
		return x
	default:
		panic("peering: unsupported projection sink type")
	}
}
