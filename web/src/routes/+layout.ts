// The Web Admin is a client-side SPA embedded in the reposuite-relay
// binary: relayd serves the static build and every /auth/* and /v1/*
// endpoint itself, so there is no server-side rendering to do here.
export const ssr = false;
export const prerender = true;
