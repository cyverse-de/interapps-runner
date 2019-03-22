FROM tutum/hello-world:latest

COPY delay.sh /delay.sh

ENTRYPOINT ["/delay.sh"]

CMD php-fpm -d variables_order="EGPCS" && (tail -F /var/log/nginx/access.log &) && exec nginx -g "daemon off;"
