package quirks

// EndpointBedrock is the endpoint the Bedrock adapter resolves the
// anthropic rules under, so an anthropic rule scoped to it applies to
// Claude on Amazon Bedrock only. It is not a RunConfig endpoint profile.
const EndpointBedrock = "bedrock"
