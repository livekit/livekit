// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

use std::{env, error::Error, fs, io, time::Duration};

use moqtap_client::draft23::connection::{ClientConfig, Connection, TransportType};
use moqtap_codec::{
    dispatch::AnySubgroupHeader,
    draft23::{
        data_stream::{SubgroupHeader, SubgroupObject},
        message::ControlMessage,
    },
    types::TrackNamespace,
    varint::VarInt,
    version::DraftVersion,
};

const PAYLOAD: &[u8] = b"LiveKit MoQ interoperability\x00\xff";

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mut args = env::args().skip(1);
    let addr = args.next().ok_or("missing QUIC server address")?;
    let cert = fs::read(args.next().ok_or("missing test certificate path")?)?;
    tokio::time::timeout(Duration::from_secs(10), round_trip(&addr, cert)).await?
}

async fn round_trip(addr: &str, cert: Vec<u8>) -> Result<(), Box<dyn Error>> {
    let config =
        ClientConfig::new(DraftVersion::Draft23, TransportType::Quic).with_ca_certs(vec![cert]);
    let mut conn = Connection::connect(addr, config).await?;
    let namespace = TrackNamespace(vec![b"livekit".to_vec(), b"interop".to_vec()]);
    let alias = VarInt::from_u64(2)?;
    let zero = VarInt::from_u64(0)?;

    let mut publication = conn
        .publish(namespace.clone(), b"up".to_vec(), alias, vec![], vec![])
        .await?;
    if !matches!(
        conn.recv_on_request_stream(&mut publication).await?,
        ControlMessage::RequestOk(_)
    ) {
        return Err(io::Error::other("PUBLISH was not accepted").into());
    }
    let header = AnySubgroupHeader::Draft23(SubgroupHeader {
        header_type: 0x14, // Explicit subgroup ID and publisher priority.
        track_alias: alias,
        group_id: zero,
        subgroup_id: zero,
        publisher_priority: Some(128),
    });
    let mut outbound = conn.open_subgroup_stream(&header).await?;
    outbound
        .write_subgroup_object(&SubgroupObject {
            object_id: zero,
            extension_headers: vec![],
            payload_length: VarInt::from_u64(PAYLOAD.len() as u64)?,
            object_status: None,
            payload: PAYLOAD.to_vec(),
        })
        .await?;
    outbound.finish().await?;

    let mut subscription = conn.subscribe(namespace, b"down".to_vec(), vec![]).await?;
    let ControlMessage::SubscribeOk(ack) = conn.recv_on_request_stream(&mut subscription).await?
    else {
        return Err(io::Error::other("SUBSCRIBE was not accepted").into());
    };
    let (header, mut inbound) = conn.accept_subgroup_stream().await?;
    let AnySubgroupHeader::Draft23(header) = header;
    let object = inbound.read_subgroup_object().await?;
    if header.track_alias != ack.track_alias
        || object.object_id != zero
        || object.payload != PAYLOAD
    {
        return Err(io::Error::other("unexpected echo object").into());
    }
    conn.close(0, b"complete");
    Ok(())
}
