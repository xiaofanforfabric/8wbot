const { Server: SshServer } = require('ssh2');
const { generateKeyPairSync } = require('crypto');
const fs = require('fs');
const path = require('path');

/**
 * Start SSH control server.
 * opts: { port, password }
 * handlers: { start: fn, stop: fn, status: fn }
 */
module.exports = function startSshServer(opts = {}, handlers = {}) {
  const port = parseInt(opts.port || process.env.SSH_PORT || 2222, 10);
  const username = opts.username || process.env.SSH_USER || 'root';
  const password = opts.password || process.env.SSH_PASSWORD || process.env.SSH_ROOT_USER_PASSWORD || '';
  const echoEnabled = typeof opts.echo !== 'undefined' ? !!opts.echo : (process.env.SSH_ECHO !== 'false');

  // Warn if no password set
  if (!password) {
    console.error('WARNING: SSH_PASSWORD is not set! SSH server may reject all connections.');
  }

  // Load or generate a persistent host key. Stored in .env as base64 to avoid newline issues.
  const envPath = path.join(__dirname, '.env');
  let privateKey = null;
  try {
    if (fs.existsSync(envPath)) {
      const envText = fs.readFileSync(envPath, 'utf8');
      const m = envText.match(/SSH_HOST_KEY_B64=(.+)/);
      if (m && m[1]) {
        privateKey = Buffer.from(m[1].trim(), 'base64').toString('utf8');
      }
    }
  } catch (e) {
    console.error('Failed to read .env for SSH host key:', e && e.message);
  }

  if (!privateKey) {
    privateKey = generateKeyPairSync('rsa', {
      modulusLength: 2048,
      publicKeyEncoding: { type: 'pkcs1', format: 'pem' },
      privateKeyEncoding: { type: 'pkcs1', format: 'pem' }
    }).privateKey;
    try {
      const b64 = Buffer.from(privateKey, 'utf8').toString('base64');
      fs.appendFileSync(envPath, `\nSSH_HOST_KEY_B64=${b64}\n`);
      console.log('Generated and persisted SSH host key to .env');
    } catch (e) {
      console.error('Failed to persist SSH host key to .env:', e && e.message);
    }
  }

  const sshServer = new SshServer({ hostKeys: [privateKey] }, (client) => {
    console.log('SSH client connected');
    let currentUser = null;
    client.on('error', (err) => console.error('SSH client socket error:', err && err.message));
    client.on('authentication', (ctx) => {
      if (ctx.method === 'password' && ctx.username === username && ctx.password === password) {
        currentUser = ctx.username;
        ctx.accept();
      } else ctx.reject();
    }).on('ready', () => {
      console.log('SSH client authenticated');
      client.on('session', (accept) => {
        const session = accept();
        session.once('shell', (acceptShell) => {
          const stream = acceptShell();
          stream.on('error', (err) => console.error('SSH stream error:', err && err.message));
          const writePrompt = () => {
            const suffix = currentUser === 'root' ? '#' : '$';
            stream.write((currentUser || 'user') + '@8wbot~' + suffix + ' ');
          };
          stream.write('Welcome to 8wbot SSH console\r\n');
          stream.write('Commands: start | stop | status | help | bot -v\r\n');
          writePrompt();

          // Buffer input until newline to avoid processing single-key events
          let inputBuffer = '';
          stream.on('data', (data) => {
            // Process input: ignore Ctrl+C (\x03) so sessions don't get killed
            let raw = data.toString('utf8');
            let hadCtrlC = false;
            if (raw.indexOf('\x03') !== -1 || raw.indexOf('\u0003') !== -1) {
              raw = raw.replace(/\x03|\u0003/g, () => { hadCtrlC = true; return ''; });
            }
            if (echoEnabled && raw.length > 0) {
              try { stream.write(raw); } catch (e) { /* ignore */ }
            }
            if (hadCtrlC) {
              inputBuffer = '';
              try { stream.write('^C\r\n'); } catch (e) { /* ignore */ }
              writePrompt();
              return;
            }
            inputBuffer += raw;
            let idx;
            while ((idx = inputBuffer.search(/\r|\n/)) !== -1) {
              let line = inputBuffer.slice(0, idx);
              if (inputBuffer[idx] === '\r' && inputBuffer[idx+1] === '\n') inputBuffer = inputBuffer.slice(idx+2);
              else inputBuffer = inputBuffer.slice(idx+1);
              line = line.replace(/\r|\n/g, '').trim();
              if (!line) { writePrompt(); continue; }
              try {
                if (line === 'bot -v') {
                  try { const ver = require('./version'); stream.write(ver + '\r\n'); }
                  catch (e) { stream.write('v0.0.0\r\n'); }
                  writePrompt();
                  continue;
                }
                if (line === 'start') { handlers.start && handlers.start(); stream.write('ok: started\r\n'); }
                else if (line === 'stop') { handlers.stop && handlers.stop(); stream.write('ok: stopped\r\n'); }
                else if (line === 'status') { const r = handlers.status ? !!handlers.status() : false; stream.write('running: ' + r + '\r\n'); }
                else if (line === 'help') { stream.write('Commands: start | stop | status | help | bot -v\r\n'); }
                else {
                  try {
                    const msg = JSON.parse(line);
                    if (msg.cmd === 'start') { handlers.start && handlers.start(msg.config); stream.write('ok: started\r\n'); }
                    else if (msg.cmd === 'stop') { handlers.stop && handlers.stop(); stream.write('ok: stopped\r\n'); }
                    else stream.write('unknown json cmd\r\n');
                  } catch (e) { stream.write('unknown command\r\n'); }
                }
              } catch (e) {
                stream.write('error: ' + e.message + '\r\n');
              }
              writePrompt();
            }
          });
          stream.on('close', () => client.end());
        });
      });
    }).on('end', () => {
      console.log('SSH client disconnected');
    });
  });

  sshServer.on('error', (e) => console.error('SSH server error:', e && e.message));

  try {
    sshServer.listen(port, '0.0.0.0', () => {
      console.log(`SSH server listening on port ${port} (user=${username})`);
    });
  } catch (e) {
    console.error('Failed to bind SSH server:', e && e.message);
  }

  return sshServer;
};
