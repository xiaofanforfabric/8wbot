package com.xiaofan.ws;

import org.java_websocket.WebSocket;
import org.java_websocket.handshake.ClientHandshake;
import org.java_websocket.server.WebSocketServer;

import java.net.InetSocketAddress;

public class SimpleWebSocketServer extends WebSocketServer {

    public SimpleWebSocketServer(int port) {
        super(new InetSocketAddress(port));
    }

    @Override
    public void onOpen(WebSocket conn, ClientHandshake handshake) {
        System.out.println("WebSocket opened: " + conn.getRemoteSocketAddress());
    }

    @Override
    public void onClose(WebSocket conn, int code, String reason, boolean remote) {
        System.out.println("WebSocket closed: " + conn.getRemoteSocketAddress() + " reason=" + reason);
    }

    @Override
    public void onMessage(WebSocket conn, String message) {
        System.out.println("Received message: " + message);
        conn.send("Echo: " + message);
    }

    @Override
    public void onError(WebSocket conn, Exception ex) {
        System.err.println("WebSocket error:");
        ex.printStackTrace();
    }

    @Override
    public void onStart() {
        System.out.println("SimpleWebSocketServer started");
    }

    public static void main(String[] args) throws Exception {
        int port = 8887;
        SimpleWebSocketServer server = new SimpleWebSocketServer(port);
        server.start();
        System.out.println("Listening on port " + port);
    }
}
