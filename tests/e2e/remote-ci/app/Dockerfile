# Trivial nginx image for the remote-ci e2e test.
# Exercises the full docker_build → push to lok8s.local → in-cluster
# pull → pod Running roundtrip on a remote VM.
FROM nginx:1.27-alpine
RUN echo "lok8s e2e remote-ci" > /usr/share/nginx/html/index.html
