// The Web Admin is a client-side SPA embedded in the reposuite-relay
// binary: relayd serves the static build and every /auth/* and /v1/*
// endpoint itself, so there is no server-side rendering to do here.
// Routes are NOT prerendered — the adapter-static fallback emits exactly
// one document (index.html), so there is no parallel route HTML that
// could bypass the document CSP.
export const ssr = false;
