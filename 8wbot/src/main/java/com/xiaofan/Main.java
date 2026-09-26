package com.xiaofan;

import org.geysermc.mcprotocollib.protocol.MinecraftProtocol;
import org.geysermc.mcprotocollib.network.Session;
import org.geysermc.mcprotocollib.network.packet.Packet;
import org.geysermc.mcprotocollib.network.ProxyInfo;
import org.geysermc.mcprotocollib.network.event.session.*;
import org.geysermc.mcprotocollib.network.session.ClientNetworkSession;

import java.net.InetSocketAddress;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import org.geysermc.mcprotocollib.protocol.packet.ingame.serverbound.player.ServerboundMovePlayerPosRotPacket;
import net.kyori.adventure.text.Component;

public class Main {
    public static void main(String[] args) throws Exception {
        String username = "wans7891"; // offline username
        String host = "bgjq.simpfun.cn";
        int port = 25565;

        System.out.println("Starting MCProtocolLib client: " + username + " -> " + host + ":" + port);

        MinecraftProtocol protocol = new MinecraftProtocol(username);

        InetSocketAddress remote = new InetSocketAddress(host, port);

        ExecutorService exec = Executors.newSingleThreadExecutor();
        ScheduledExecutorService movementExec = Executors.newSingleThreadScheduledExecutor();

        // position state (updated from server packets)
        final double[] pos = new double[]{0.0, 0.0, 0.0}; // x,y,z
        final float[] rot = new float[]{0.0f, 0.0f}; // yaw, pitch
        // velocity applied by server (knockback etc.) — preserved between ticks
        final double[] vel = new double[]{0.0, 0.0, 0.0};

        // bindAddress and proxy can be null for default behaviour
        ClientNetworkSession session = new ClientNetworkSession(remote, protocol, exec, null, null);

        final AtomicBoolean movementStarted = new AtomicBoolean(false);

        session.addListener(new SessionListener() {
            @Override
            public void packetReceived(Session s, Packet packet) {
                String name = packet.getClass().getSimpleName();
                System.out.println("Packet received: " + name);
                // update position when server sends authoritative player position
                if ("ClientboundPlayerPositionPacket".equals(name)) {
                    try {
                        Object posObj = packet.getClass().getMethod("getPosition").invoke(packet);
                        if (posObj != null) {
                            try {
                                java.lang.reflect.Method mx = posObj.getClass().getMethod("getX");
                                java.lang.reflect.Method my = posObj.getClass().getMethod("getY");
                                java.lang.reflect.Method mz = posObj.getClass().getMethod("getZ");
                                Object ox = mx.invoke(posObj);
                                Object oy = my.invoke(posObj);
                                Object oz = mz.invoke(posObj);
                                pos[0] = ((Number) ox).doubleValue();
                                pos[1] = ((Number) oy).doubleValue();
                                pos[2] = ((Number) oz).doubleValue();
                                System.out.println(String.format("Updated position from server: x=%.4f y=%.4f z=%.4f", pos[0], pos[1], pos[2]));
                            } catch (NoSuchMethodException ignored) {}
                        }
                    } catch (Throwable t) {
                        // ignore
                    }
                } else if ("ClientboundSetEntityMotionPacket".equals(name) || "ClientboundSetEntityMotion".equals(name) || name.contains("SetEntityMotion")) {
                    // server applied motion/knockback to the player — try to extract and apply
                    try {
                        Object motion = null;
                        // try several likely method names
                        for (String m : new String[]{"getMotion", "getDeltaMovement", "getVelocity", "motion"}) {
                            try {
                                java.lang.reflect.Method mm = packet.getClass().getMethod(m);
                                motion = mm.invoke(packet);
                                if (motion != null) break;
                            } catch (NoSuchMethodException ignored) {}
                        }
                        if (motion != null) {
                            // motion may be a Vector3d-like object or an array
                            try {
                                java.lang.reflect.Method mx = motion.getClass().getMethod("getX");
                                java.lang.reflect.Method my = motion.getClass().getMethod("getY");
                                java.lang.reflect.Method mz = motion.getClass().getMethod("getZ");
                                vel[0] = ((Number) mx.invoke(motion)).doubleValue();
                                vel[1] = ((Number) my.invoke(motion)).doubleValue();
                                vel[2] = ((Number) mz.invoke(motion)).doubleValue();
                                System.out.println(String.format("Received server motion: vx=%.4f vy=%.4f vz=%.4f", vel[0], vel[1], vel[2]));
                            } catch (NoSuchMethodException ex) {
                                // maybe it's a double[]
                                if (motion instanceof double[]) {
                                    double[] ma = (double[]) motion;
                                    if (ma.length >= 3) {
                                        vel[0] = ma[0]; vel[1] = ma[1]; vel[2] = ma[2];
                                    }
                                }
                            }
                        }
                    } catch (Throwable t) {
                        // ignore
                    }
                }
                // respond to server keepalive/ping if present
                if (name.contains("Ping") || name.toLowerCase().contains("keepalive")) {
                    try {
                        Object idObj = null;
                        for (String m : new String[]{"getId", "getPingId", "getRandomId", "getKey", "getKeepAliveId", "getPing"}) {
                            try {
                                java.lang.reflect.Method mm = packet.getClass().getMethod(m);
                                idObj = mm.invoke(packet);
                                if (idObj != null) break;
                            } catch (NoSuchMethodException ignored) {}
                        }
                        long id = 0L;
                        if (idObj instanceof Number) {
                            id = ((Number) idObj).longValue();
                        } else if (idObj instanceof long[]) {
                            long[] la = (long[]) idObj; if (la.length>0) id = la[0];
                        } else if (idObj != null) {
                            try { id = Long.parseLong(idObj.toString()); } catch (Throwable ignored) {}
                        }
                        System.out.println("Received keepalive/ping id=" + id + " — attempting reply");
                        String[] candidates = new String[]{
                                "org.geysermc.mcprotocollib.protocol.packet.ingame.serverbound.play.ServerboundPongPacket",
                                "org.geysermc.mcprotocollib.protocol.packet.ingame.serverbound.play.ServerboundKeepAlivePacket",
                                "org.geysermc.mcprotocollib.protocol.packet.ingame.serverbound.play.ServerboundClientKeepAlivePacket"
                        };
                        boolean replied = false;
                        for (String clsName : candidates) {
                            try {
                                Class<?> cls = Class.forName(clsName);
                                java.lang.reflect.Constructor<?> ctor = null;
                                try { ctor = cls.getConstructor(long.class); } catch (NoSuchMethodException ignored) {}
                                if (ctor == null) try { ctor = cls.getConstructor(Long.class); } catch (NoSuchMethodException ignored) {}
                                if (ctor == null) try { ctor = cls.getConstructor(int.class); } catch (NoSuchMethodException ignored) {}
                                if (ctor == null) try { ctor = cls.getConstructor(Integer.class); } catch (NoSuchMethodException ignored) {}
                                if (ctor != null) {
                                    Object reply = null;
                                    Class<?> param = ctor.getParameterTypes()[0];
                                    if (param == long.class || param == Long.class) reply = ctor.newInstance(id);
                                    else if (param == int.class || param == Integer.class) reply = ctor.newInstance((int) id);
                                    if (reply != null) {
                                        session.send((Packet) reply, () -> {});
                                        System.out.println("Sent keepalive reply using " + clsName);
                                        replied = true;
                                        break;
                                    }
                                }
                            } catch (ClassNotFoundException cnf) {
                                // try next candidate
                            }
                        }
                        if (!replied) System.out.println("No suitable serverbound keepalive class found to reply");
                        if (!replied) {
                            try {
                                String base = "org";
                                java.util.Enumeration<java.net.URL> resources = Thread.currentThread().getContextClassLoader().getResources(base);
                                java.util.List<String> found = new java.util.ArrayList<>();
                                while (resources.hasMoreElements()) {
                                    java.net.URL url = resources.nextElement();
                                    String proto = url.getProtocol();
                                    if ("jar".equals(proto)) {
                                        String path = url.getPath();
                                        String jarPath = path.substring(5, path.indexOf("!"));
                                        try (java.util.jar.JarFile jf = new java.util.jar.JarFile(java.net.URLDecoder.decode(jarPath, "UTF-8"))) {
                                            java.util.Enumeration<java.util.jar.JarEntry> entries = jf.entries();
                                            while (entries.hasMoreElements()) {
                                                java.util.jar.JarEntry je = entries.nextElement();
                                                String n = je.getName();
                                                if (n.endsWith(".class")) {
                                                    String lower = n.toLowerCase();
                                                    if ((n.contains("Pong") || lower.contains("keepalive") || n.contains("Ping")) && lower.contains("serverbound") && lower.contains("mcprotocollib")) {
                                                        String cls = n.replace('/', '.').substring(0, n.length() - 6);
                                                        found.add(cls);
                                                    }
                                                }
                                            }
                                        }
                                    } else if ("file".equals(proto)) {
                                        java.io.File dir = new java.io.File(url.toURI());
                                        java.util.Queue<java.io.File> q = new java.util.ArrayDeque<>();
                                        q.add(dir);
                                        while (!q.isEmpty()) {
                                            java.io.File f = q.poll();
                                            for (java.io.File c : f.listFiles()) {
                                                if (c.isDirectory()) q.add(c);
                                                else if (c.getName().endsWith(".class")) {
                                                    String fileName = c.getName();
                                                    String lower = fileName.toLowerCase();
                                                    if ((fileName.contains("Pong") || lower.contains("keepalive") || fileName.contains("Ping"))) {
                                                        String rel = c.getAbsolutePath().replace("\\", "/");
                                                        int idx = rel.indexOf("org/");
                                                        if (idx >= 0) {
                                                            String cls = rel.substring(idx).replace('/', '.').replace(".class", "");
                                                            if (cls.toLowerCase().contains("serverbound") && cls.toLowerCase().contains("mcprotocollib")) found.add(cls);
                                                        }
                                                    }
                                                }
                                            }
                                        }
                                    }
                                }
                                for (String clsName : found) {
                                    try {
                                        Class<?> cls = Class.forName(clsName);
                                        if (!cls.getName().contains("serverbound")) continue;
                                        java.lang.reflect.Constructor<?> ctor = null;
                                        try { ctor = cls.getConstructor(long.class); } catch (NoSuchMethodException ignored) {}
                                        if (ctor == null) try { ctor = cls.getConstructor(Long.class); } catch (NoSuchMethodException ignored) {}
                                        if (ctor == null) try { ctor = cls.getConstructor(int.class); } catch (NoSuchMethodException ignored) {}
                                        if (ctor == null) try { ctor = cls.getConstructor(Integer.class); } catch (NoSuchMethodException ignored) {}
                                        if (ctor != null) {
                                            Object reply = null;
                                            Class<?> param = ctor.getParameterTypes()[0];
                                            if (param == long.class || param == Long.class) reply = ctor.newInstance(id);
                                            else if (param == int.class || param == Integer.class) reply = ctor.newInstance((int) id);
                                            if (reply != null) {
                                                session.send((Packet) reply, () -> {});
                                                System.out.println("Sent keepalive reply using discovered class " + clsName);
                                                replied = true;
                                                break;
                                            }
                                        }
                                    } catch (Throwable ignored) {}
                                }
                                if (!replied) System.out.println("Runtime scan didn't find a usable serverbound keepalive class");
                            } catch (Throwable t) {
                                System.err.println("Keepalive runtime scan failed: " + t);
                            }
                        }
                    } catch (Throwable t) {
                        System.err.println("Failed to handle keepalive ping: " + t);
                        t.printStackTrace(System.err);
                    }
                }
                // start periodic movement once we see server play packets (mapping should be ready)
                try {
                    if (!movementStarted.get() && (name.contains("JoinGame") || "ClientboundPlayerPositionPacket".equals(name))) {
                        if (movementStarted.compareAndSet(false, true)) {
                            System.out.println("Entered play state — scheduling movement start in 10s");
                            movementExec.scheduleAtFixedRate(() -> {
                                try {
                                    double dt = 0.05; // 50ms
                                    pos[0] += vel[0] * dt;
                                    pos[1] += vel[1] * dt;
                                    pos[2] += vel[2] * dt;
                                    vel[0] *= 0.85; vel[1] *= 0.85; vel[2] *= 0.85;
                                    double dx = (Math.random() - 0.5) * 0.04;
                                    double dz = (Math.random() - 0.5) * 0.04;
                                    pos[0] += dx; pos[2] += dz;
                                    rot[0] += (float) ((Math.random() - 0.5) * 2.0);
                                    boolean onGround = Math.abs(vel[1]) < 0.02;
                                    ServerboundMovePlayerPosRotPacket move = new ServerboundMovePlayerPosRotPacket(onGround, false, pos[0], pos[1], pos[2], rot[0], rot[1]);
                                    session.send(move, () -> {});
                                    System.out.println(String.format("Sent move packet: x=%.4f y=%.4f z=%.4f (vx=%.4f vy=%.4f vz=%.4f)", pos[0], pos[1], pos[2], vel[0], vel[1], vel[2]));
                                } catch (Throwable t) {
                                    System.err.println("Failed to send move packet: " + t);
                                }
                            }, 10_000, 50, TimeUnit.MILLISECONDS);
                        }
                    }
                } catch (Throwable ignored) {}
            }

            @Override
            public void packetSending(PacketSendingEvent event) {
                System.out.println("Packet sending: " + event);
            }

            @Override
            public void packetSent(Session s, Packet packet) {
                System.out.println("Packet sent: " + packet.getClass().getSimpleName());
            }

            @Override
            public void packetError(PacketErrorEvent event) {
                System.err.println("Packet error: " + event);
                try {
                    Throwable cause = event.getCause();
                    if (cause != null) {
                        System.err.println("PacketError cause:");
                        cause.printStackTrace(System.err);
                    }
                } catch (Throwable ignored) {}
            }

            @Override
            public void connected(ConnectedEvent event) {
                System.out.println("Connected to server: " + event);
            }

            @Override
            public void disconnecting(DisconnectingEvent event) {
                System.out.println("Disconnecting: " + event);
                // try to print kick reason if present on the event
                try {
                    Object reason = null;
                    for (String m : new String[]{"getReason", "getDisconnectReason", "getMessage", "reason"}) {
                        try {
                            java.lang.reflect.Method mm = event.getClass().getMethod(m);
                            reason = mm.invoke(event);
                            if (reason != null) break;
                        } catch (NoSuchMethodException ignored) {
                        }
                    }
                    if (reason != null) {
                        System.out.println("Kick reason (disconnecting): " + reason.toString());
                    } else {
                        System.out.println("Kick reason (disconnecting): <not available>");
                        try {
                            System.out.println("DisconnectingEvent class: " + event.getClass().getName());
                            java.lang.reflect.Method[] methods = event.getClass().getMethods();
                            System.out.println("Available methods on DisconnectingEvent:");
                            for (java.lang.reflect.Method m : methods) {
                                System.out.println(" - " + m.getName());
                            }
                            Throwable cause = null;
                            try { cause = (Throwable) event.getClass().getMethod("getCause").invoke(event); } catch (Throwable ignored2) {}
                            if (cause != null) {
                                System.out.println("Disconnecting event cause:");
                                cause.printStackTrace(System.out);
                            }
                        } catch (Throwable t) {
                            System.err.println("Failed to introspect DisconnectingEvent: " + t);
                        }
                    }
                } catch (Throwable t) {
                    System.err.println("Failed to read kick reason: " + t);
                }
            }

            @Override
            public void disconnected(DisconnectedEvent event) {
                System.out.println("Disconnected: " + event);
                // try to print kick reason if present on the event
                try {
                    Object reason = null;
                    for (String m : new String[]{"getReason", "getDisconnectReason", "getMessage", "reason"}) {
                        try {
                            java.lang.reflect.Method mm = event.getClass().getMethod(m);
                            reason = mm.invoke(event);
                            if (reason != null) break;
                        } catch (NoSuchMethodException ignored) {
                        }
                    }
                    if (reason != null) {
                        System.out.println("Kick reason (disconnected): " + reason.toString());
                    } else {
                        System.out.println("Kick reason: <not available>");
                        try {
                            System.out.println("DisconnectedEvent class: " + event.getClass().getName());
                            java.lang.reflect.Method[] methods = event.getClass().getMethods();
                            System.out.println("Available methods on DisconnectedEvent:");
                            for (java.lang.reflect.Method m : methods) {
                                System.out.println(" - " + m.getName());
                            }
                            Throwable cause = null;
                            try { cause = (Throwable) event.getClass().getMethod("getCause").invoke(event); } catch (Throwable ignored2) {}
                            if (cause != null) {
                                System.out.println("Disconnected event cause:");
                                cause.printStackTrace(System.out);
                            }
                        } catch (Throwable t) {
                            System.err.println("Failed to introspect DisconnectedEvent: " + t);
                        }
                    }
                } catch (Throwable t) {
                    System.err.println("Failed to read kick reason: " + t);
                }
            }
        });

        // connect (this will perform an offline login using the provided username)
        session.connect();

        // keep running for 30 seconds to observe events, then disconnect
        Thread.sleep(30_000);

        session.disconnect((Component) null, null);
        movementExec.shutdownNow();
        exec.shutdownNow();
    }
}
