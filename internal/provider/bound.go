package provider

// ModelBound is implemented by a Provider that serves one fixed model. The
// router chooses a model per run, but nothing in a Stream call carries that
// choice, so such a provider calls the model it was configured with whatever
// the router chose. The model a run is priced and recorded under has to be the
// one that is actually called, or the cost ceiling is enforced against a
// figure that is not on the bill.
type ModelBound interface {
	BoundModel() string
}

// ServedModel is the model a run that the router sent to routed will actually
// be served by on p. A provider bound to a model serves that one. A provider
// with no model of its own (the scripted fake) serves whatever it is asked, so
// the routed id stands, and is a price class.
func ServedModel(p Provider, routed string) string {
	if b, ok := p.(ModelBound); ok {
		if m := b.BoundModel(); m != "" {
			return m
		}
	}
	return routed
}
