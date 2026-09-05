FROM alpine:3.21

# lftp is deliberately moved off the non-interactive PATH so the relay's startup
# probe (internal/nas/nas.go, lftpCandidates) is exercised rather than trivially
# satisfied. That is exactly the Synology/Entware shape.
#
# adduser -D leaves "!" in /etc/shadow, which sshd reads as a locked account and
# refuses even public-key auth for.
RUN apk add --no-cache openssh-server openssh-client openssh-sftp-server openssh-keygen \
        lftp sshpass coreutils procps \
 && mkdir -p /opt/bin && mv /usr/bin/lftp /opt/bin/lftp.real \
 && adduser -D -u 1000 nas \
 && sed -i 's|^nas:!:|nas:*:|' /etc/shadow

COPY entrypoint.sh /entrypoint.sh
COPY lftp-wrapper.sh /opt/bin/lftp
RUN chmod +x /entrypoint.sh /opt/bin/lftp
EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
