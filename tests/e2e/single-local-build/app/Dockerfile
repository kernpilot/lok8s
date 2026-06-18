# Trivial nginx-based image so the e2e test exercises the full
# docker_build → push to lok8s.local → in-cluster pull → pod
# Running roundtrip without depending on a project-specific source
# tree.
FROM nginx:1.27-alpine

# Stamp the build with a marker file Tilt can verify after deploy.
RUN echo "lok8s e2e single-local-build" > /usr/share/nginx/html/index.html
